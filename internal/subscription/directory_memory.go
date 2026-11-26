package subscription

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
)

type memoryDirectory struct {
	mu      sync.Mutex
	groupID int64
	groups  map[int64]ownedGroup
	saved   map[[2]int64]map[int64]struct{}
	reports []Report
}

type ownedGroup struct {
	Group
	owner int64
}

func (m *memoryDirectory) SubscriptionGroups(ctx context.Context, owner int64, kind int) ([]Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	groups := []Group{}
	for _, group := range m.groups {
		if group.owner == owner && group.Kind == kind {
			copy := group.Group
			copy.Users = slices.Clone(copy.Users)
			groups = append(groups, copy)
		}
	}
	if len(groups) == 0 && kind < CustomGroup {
		groups = append(groups, m.newGroup(owner, kind, ""))
	}
	slices.SortFunc(groups, func(a, b Group) int { return cmp.Compare(a.ID, b.ID) })
	return groups, nil
}

func (m *memoryDirectory) SubscriptionGroup(ctx context.Context, owner, id int64) (Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Group{}, err
	}
	group, found := m.groups[id]
	if !found || group.owner != owner {
		return Group{}, ErrNotFound
	}
	group.Users = slices.Clone(group.Users)
	return group.Group, nil
}

func (m *memoryDirectory) newGroup(owner int64, kind int, name string) Group {
	if m.groups == nil {
		m.groups = map[int64]ownedGroup{}
	}
	m.groupID++
	group := Group{ID: m.groupID, Kind: kind, Name: name}
	m.groups[group.ID] = ownedGroup{Group: group, owner: owner}
	return group
}

func (m *memoryDirectory) CreateSubscriptionGroup(ctx context.Context, owner int64, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.newGroup(owner, CustomGroup, name)
	return nil
}

func (m *memoryDirectory) ChangeSubscriptionGroup(ctx context.Context, owner, id int64, change GroupChange) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	group, found := m.groups[id]
	if !found || group.owner != owner {
		return ErrNotFound
	}
	if change.Delete {
		delete(m.groups, id)
		return nil
	}
	if change.Clear {
		group.Users = nil
	}
	if change.Name != nil {
		group.Name = *change.Name
	}
	m.groups[id] = group
	return nil
}

func (m *memoryDirectory) SavedSubscriptionUsers(ctx context.Context, owner int64, list int) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.saved[[2]int64{owner, int64(list)}])), ctx.Err()
}

func (m *memoryDirectory) ClearSubscriptionList(ctx context.Context, owner int64, list int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(m.saved, [2]int64{owner, int64(list)})
	return nil
}

func (m *memoryDirectory) ChangeSubscriptionMember(ctx context.Context, owner int64, change MemberChange) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var group ownedGroup
	if change.GroupID != 0 {
		var found bool
		group, found = m.groups[change.GroupID]
		if !found || group.owner != owner {
			return ErrNotFound
		}
	}
	person := change.PersonID
	if change.Add && change.List != Blacklist {
		if _, blocked := m.saved[[2]int64{owner, Blacklist}][person]; blocked {
			return ErrForbidden
		}
	}
	if change.List != 0 {
		key := [2]int64{owner, int64(change.List)}
		if change.Add {
			if m.saved == nil {
				m.saved = map[[2]int64]map[int64]struct{}{}
			}
			if m.saved[key] == nil {
				m.saved[key] = map[int64]struct{}{}
			}
			m.saved[key][person] = struct{}{}
		} else {
			delete(m.saved[key], person)
		}
	}
	if change.Add && change.List == Blacklist {
		delete(m.saved[[2]int64{owner, Friends}], person)
		for id, entry := range m.groups {
			if entry.owner == owner {
				entry.Users = removeGroupMember(entry.Users, person)
				m.groups[id] = entry
			}
		}
	}
	if change.GroupID != 0 {
		if change.Add {
			if !slices.Contains(group.Users, person) {
				group.Users = append(group.Users, person)
			}
		} else {
			group.Users = removeGroupMember(group.Users, person)
		}
		m.groups[group.ID] = group
	}
	return nil
}

func removeGroupMember(users []int64, person int64) []int64 {
	index := slices.Index(users, person)
	if index < 0 {
		return users
	}
	return slices.Delete(users, index, index+1)
}

func (m *memoryDirectory) ReportSubscription(ctx context.Context, report Report) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	report.URLs = slices.Clone(report.URLs)
	m.reports = append(m.reports, report)
	return nil
}

func (*memoryDirectory) SubscriptionExperience(ctx context.Context, _, _ int64) (int64, int64, error) {
	return 0, 0, ctx.Err()
}
