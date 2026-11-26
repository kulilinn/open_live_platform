package stt

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/rtc"
)

var ErrLanguage = errors.New("provide a transcription language")

type RoomState struct {
	OwnerID       int64
	ParticipantID int64
	LiveID        int64
	Online        bool
}

func (r RoomState) allows(actor int64) bool {
	return actor != 0 && (r.OwnerID == actor || r.ParticipantID == actor)
}

type Task struct {
	RoomID, LiveID int64
	Version        string
	RemoteID       string
	BuilderToken   string
	Sequence       int64
}

type Store interface {
	ChangeSTTTask(context.Context, int64, func(RoomState, *Task) error) error
	StaleSTTTaskRooms(context.Context) ([]int64, error)
}

type Service struct {
	agora *agora
	store Store
}

func New(config Config, signer rtc.Signer, rooms live.RoomStore, store Store) *Service {
	if store == nil {
		store = &memoryStore{rooms: rooms, tasks: make(map[int64]Task)}
	}
	return &Service{agora: newAgora(config, signer), store: store}
}

func (s *Service) Enabled() bool          { return s.agora.enabled() }
func (s *Service) CallbackSecret() string { return s.agora.config.CallbackSecret }

func (s *Service) Start(ctx context.Context, roomID, actor int64, langs []string, automatic bool) error {
	if err := checkLanguages(langs); err != nil {
		return err
	}
	version := s.agora.config.Version
	if automatic {
		version = Agent
	}
	var launched Task
	err := s.store.ChangeSTTTask(ctx, roomID, func(room RoomState, task *Task) error {
		if !room.allows(actor) {
			return live.ErrForbidden
		}
		if !room.Online {
			return live.ErrRoomClosed
		}
		if task.RemoteID != "" && task.LiveID == room.LiveID {
			status, err := s.agora.status(ctx, *task)
			if err != nil {
				return err
			}
			if status != "STOPPED" && status != "FAILED" {
				return nil
			}
		} else if task.RemoteID != "" {
			if err := s.agora.stop(ctx, *task); err != nil {
				return err
			}
		}
		var err error
		launched, err = s.agora.start(ctx, roomID, room.LiveID, version, langs)
		if err == nil {
			*task = launched
		}
		return err
	})
	if err != nil && launched.RemoteID != "" {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		return errors.Join(err, s.agora.stop(cleanupCtx, launched))
	}
	return err
}

func (s *Service) Query(ctx context.Context, roomID int64) (string, error) {
	status := "STOPPED"
	err := s.store.ChangeSTTTask(ctx, roomID, func(room RoomState, task *Task) error {
		if task.RemoteID == "" || !room.Online || task.LiveID != room.LiveID {
			return nil
		}
		var err error
		status, err = s.agora.status(ctx, *task)
		return err
	})
	return status, err
}

func (s *Service) Update(ctx context.Context, roomID, actor int64, langs []string) error {
	if err := checkLanguages(langs); err != nil {
		return err
	}
	return s.store.ChangeSTTTask(ctx, roomID, func(room RoomState, task *Task) error {
		if !room.allows(actor) {
			return live.ErrForbidden
		}
		if task.RemoteID == "" || !room.Online || task.LiveID != room.LiveID {
			return live.ErrRoomNotFound
		}
		task.Sequence++
		return s.agora.update(ctx, *task, langs)
	})
}

func (s *Service) Stop(ctx context.Context, roomID, actor int64) error {
	return s.store.ChangeSTTTask(ctx, roomID, func(room RoomState, task *Task) error {
		if !room.allows(actor) {
			return live.ErrForbidden
		}
		return s.stopTask(ctx, task)
	})
}

func (s *Service) stopTask(ctx context.Context, task *Task) error {
	if task.RemoteID == "" {
		return nil
	}
	if err := s.agora.stop(ctx, *task); err != nil {
		return err
	}
	*task = Task{}
	return nil
}

func (s *Service) AgentEnded(ctx context.Context, roomID int64, agentID string) error {
	err := s.store.ChangeSTTTask(ctx, roomID, func(_ RoomState, task *Task) error {
		if task.Version == Agent && task.RemoteID == agentID {
			*task = Task{}
		}
		return nil
	})
	if errors.Is(err, live.ErrRoomNotFound) {
		return nil
	}
	return err
}

func (s *Service) Sweep(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	rooms, err := s.store.StaleSTTTaskRooms(ctx)
	if err != nil {
		return err
	}
	var sweepErr error
	for _, room := range rooms {
		err := s.store.ChangeSTTTask(ctx, room, func(room RoomState, task *Task) error {
			if task.RemoteID == "" || (room.Online && task.LiveID == room.LiveID) {
				return nil
			}
			return s.stopTask(ctx, task)
		})
		sweepErr = errors.Join(sweepErr, err)
		if ctx.Err() != nil {
			break
		}
	}
	return sweepErr
}

func checkLanguages(langs []string) error {
	if len(langs) == 0 {
		return ErrLanguage
	}
	for _, lang := range langs {
		if strings.TrimSpace(lang) == "" {
			return ErrLanguage
		}
	}
	return nil
}

type memoryStore struct {
	rooms live.RoomStore
	locks [32]sync.Mutex
	mu    sync.Mutex
	tasks map[int64]Task
}

func (s *memoryStore) ChangeSTTTask(ctx context.Context, roomID int64, fn func(RoomState, *Task) error) error {
	lock := &s.locks[uint64(roomID)%uint64(len(s.locks))]
	lock.Lock()
	defer lock.Unlock()
	room, err := s.rooms.Info(ctx, roomID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	task := s.tasks[roomID]
	s.mu.Unlock()
	previous := task
	if err := fn(RoomState{OwnerID: room.OwnerID, LiveID: room.Session.LiveID, Online: room.Online}, &task); err != nil {
		return err
	}
	if task != previous && task.RemoteID != "" {
		current, err := s.rooms.Info(ctx, roomID)
		if err != nil {
			return err
		}
		if !current.Online || current.Session.LiveID != task.LiveID {
			return live.ErrRoomClosed
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if task.RemoteID == "" {
		delete(s.tasks, roomID)
	} else {
		s.tasks[roomID] = task
	}
	return nil
}

func (s *memoryStore) StaleSTTTaskRooms(ctx context.Context) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.tasks))
	for id := range s.tasks {
		ids = append(ids, id)
	}
	return ids, ctx.Err()
}
