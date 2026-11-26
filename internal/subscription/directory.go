package subscription

import "context"

const (
	FreeGroup        = 0
	BasicGroup       = 1
	AdvancedGroup    = 2
	CustomGroup      = 3
	SubscriptionList = 1
	Friends          = 2
	Blacklist        = 3
)

type Group struct {
	ID    int64
	Name  string
	Kind  int
	Users []int64
}

type GroupChange struct {
	Name          *string
	Clear, Delete bool
}

type MemberChange struct {
	GroupID, PersonID int64
	List              int
	Add               bool
}

type Report struct {
	Reporter, PersonID, PostID int64
	Kind                       int
	URLs                       []string
	Remark                     string
}

type DirectoryStore interface {
	SubscriptionGroups(context.Context, int64, int) ([]Group, error)
	SubscriptionGroup(ctx context.Context, owner, id int64) (Group, error)
	CreateSubscriptionGroup(ctx context.Context, owner int64, name string) error
	ChangeSubscriptionGroup(ctx context.Context, owner, id int64, change GroupChange) error
	SavedSubscriptionUsers(ctx context.Context, owner int64, list int) ([]int64, error)
	ChangeSubscriptionMember(ctx context.Context, owner int64, change MemberChange) error
	ClearSubscriptionList(ctx context.Context, owner int64, list int) error
	ReportSubscription(context.Context, Report) error
	SubscriptionExperience(ctx context.Context, fanID, anchorID int64) (platform, fan int64, err error)
}

var platformThresholds = []int64{
	0, 1, 2, 3, 5, 8, 10, 13, 19, 26, 40, 50, 60, 90, 110, 140, 190, 250, 320, 420, 550, 800, 1000,
	1300, 1600, 2200, 3000, 3800, 4900, 6300, 8600, 10000, 15000, 19000, 25000, 33000, 43000, 56000,
	79000, 93000, 100000, 130000, 160000, 220000, 230000, 260000, 290000, 330000, 380000, 430000,
	490000, 650000, 690000, 1150000, 1260000, 1360000, 1500000, 1600000, 1900000, 2300000, 2600000,
	2900000, 3300000, 4500000, 5500000, 6500000, 7300000, 7900000, 9800000, 10800000, 14000000,
	15000000, 16000000, 18000000, 26000000, 30000000,
}

var fanThresholds = []int64{0, 0, 60, 150, 260, 420, 640, 910, 1280, 1850, 2700, 5600, 9900, 17000,
	24200, 31300, 59900, 131300, 198600, 271500, 414300}

func ExperienceLevels(platform, fan int64) (int, int) {
	return experienceLevel(platform, platformThresholds), experienceLevel(fan, fanThresholds)
}

func experienceLevel(points int64, thresholds []int64) int {
	if points <= 0 {
		return 0
	}
	level := 0
	for i := 1; i < len(thresholds) && points >= thresholds[i]; i++ {
		level = i
	}
	return level
}
