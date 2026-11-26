package auth

// Profile holds the public identity fields on LiveMedia's user record.
type Profile struct {
	Name       string
	Gender     int
	Photo      string
	Verified   bool
	Popularity int
	UserType   string
	NSFW       *bool
	IsAnchor   bool
	ToyInfo    string
	Banner     string
	SearchName string
	GroupRoom  string
}
