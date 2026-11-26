package anchor

import (
	"context"
	"time"
)

type TaskTarget int

const (
	TaskFollow TaskTarget = iota
	TaskPaidFan
	TaskGiftSender
	TaskRocket
	TaskLiveMinutes
	TaskTargetCount
)

const TaskEligibility = 180 * 24 * time.Hour

var japanTaskTime = time.FixedZone("JST", 9*60*60)

func TaskDay(at time.Time) string {
	return at.In(japanTaskTime).Format(time.DateOnly)
}

type TaskDefinition struct {
	ID        int64      `json:"id"`
	Target    TaskTarget `json:"define"`
	Title     string     `json:"title"`
	Reward    int64      `json:"gold"`
	Progress  int64      `json:"x"`
	Condition int64      `json:"y"`
}

var defaultStarTasks = [...]TaskDefinition{
	{ID: 1001, Target: TaskFollow, Title: "task_describe_1001", Reward: 158, Condition: 10},
	{ID: 1002, Target: TaskPaidFan, Title: "task_describe_1002", Reward: 790, Condition: 3},
	{ID: 1003, Target: TaskGiftSender, Title: "task_describe_1003", Reward: 158, Condition: 3},
	{ID: 1004, Target: TaskRocket, Title: "task_describe_1004", Reward: 1580, Condition: 1},
	{ID: 1005, Target: TaskLiveMinutes, Title: "task_describe_1005", Reward: 790, Condition: 120},
	{ID: 1006, Target: TaskLiveMinutes, Title: "task_describe_1006", Reward: 790, Condition: 240},
}

type TaskProgress struct {
	Gold   int64            `json:"taskGold"`
	Length int              `json:"taskLang"`
	Done   int              `json:"taskOk"`
	Tasks  []TaskDefinition `json:"taskList"`
}

func StarTasks(progress [TaskTargetCount]int64, balance int64) TaskProgress {
	tasks := make([]TaskDefinition, 0, len(defaultStarTasks))
	var done int
	for _, definition := range defaultStarTasks {
		definition.Progress = min(progress[definition.Target], definition.Condition)
		if definition.Progress == definition.Condition {
			done++
		}
		tasks = append(tasks, definition)
	}
	return TaskProgress{Gold: balance, Length: len(tasks), Done: done, Tasks: tasks}
}

type TaskStore interface {
	Tasks(ctx context.Context, anchorID int64, now time.Time) (TaskProgress, error)
	ReconcileTasks(ctx context.Context, anchorID int64, now time.Time) error
	StarTaskStarted(ctx context.Context, anchorID int64) (*time.Time, error)
}
