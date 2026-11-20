package extension

// FeatureContribution declares a linked Go feature. The host owns activation,
// rendering and saved settings. This declaration is local to the linked host.
type FeatureContribution struct {
	ID, API    string
	Requires   []string
	Essential  bool
	Name, Sub  string
	One, Many  string
	InUse      func() (int, error)
	Activate   func(FeatureLifetime) error
	Client     []string
	Parts      []FeaturePart
	Settings   []FeatureSetting
	Routing    map[string]LinkedRoutingStrategy
	SettingsAt *FeatureSettingsLocation
	OpenPage   string
}

// FeatureLifetime receives cleanup for registrations made during activation.
// Register cleanup as each step succeeds. The host runs it in reverse order
// on failed activation, disable, or shutdown.
type FeatureLifetime interface {
	OnDispose(func())
}

// FeatureSettingsLocation names a Settings tab and the HTML id of its row.
type FeatureSettingsLocation struct {
	Tab   string
	RowID string
}

type FeatureSlot string

const (
	FeatureNavigationPage  FeatureSlot = "nav.page"
	FeatureSettingsSection FeatureSlot = "settings.section"
	FeatureInlinePart      FeatureSlot = "part"
)

type FeaturePart struct {
	Slot FeatureSlot
	Key  string
}

type FeatureSettingKind string

const (
	FeatureSettingBoolean FeatureSettingKind = "boolean"
	FeatureSettingInteger FeatureSettingKind = "integer"
	FeatureSettingNumber  FeatureSettingKind = "number"
	FeatureSettingString  FeatureSettingKind = "string"
)

// FeatureSetting declares one scalar setting. Default and Enum values must
// have the declared Kind. Integer values must fit the exact JSON integer range
// (-2^53+1 through 2^53-1). Minimum and Maximum apply to numeric kinds.
type FeatureSetting struct {
	Name        string             `json:"name"`
	Kind        FeatureSettingKind `json:"kind"`
	Title       string             `json:"title"`
	Description string             `json:"description,omitempty"`
	Default     any                `json:"default"`
	Enum        []any              `json:"enum,omitempty"`
	Minimum     *float64           `json:"minimum,omitempty"`
	Maximum     *float64           `json:"maximum,omitempty"`
}
