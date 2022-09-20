package urlapi

// ElectionMetadata contains the process metadata fields as stored on ipfs
type ElectionMetadata struct {
	Description LanguageString         `json:"description,omitempty"`
	Media       ProcessMedia           `json:"media,omitempty"`
	Meta        interface{}            `json:"meta,omitempty"`
	Questions   []Question             `json:"questions,omitempty"`
	Results     ElectionResultsDetails `json:"results,omitempty"`
	Title       LanguageString         `json:"title,omitempty"`
	Version     string                 `json:"version,omitempty"`
}

// LanguageString is a wrapper for multi-language strings, specified in metadata.
//  example {"default": "hello", "en": "hello", "es": "hola"}
type LanguageString map[string]string

// ProcessMedia holds the process metadata's header and streamURI
type ProcessMedia struct {
	Header    string `json:"header,omitempty"`
	StreamURI string `json:"streamUri,omitempty"`
}

// ElectionResultsDetails describes how a process results should be displayed and aggregated
// Remove?
type ElectionResultsDetails struct {
	Aggregation string `json:"aggregation,omitempty"`
	Display     string `json:"display,omitempty"`
}

// Question contains metadata for one single question of a process
type Question struct {
	Choices     []ChoiceMetadata `json:"choices"`
	Description LanguageString   `json:"description"`
	Title       LanguageString   `json:"title"`
}

// ChoiceMetadata contains metadata for one choice of a question
type ChoiceMetadata struct {
	Title LanguageString `json:"title,omitempty"`
	Value uint32         `json:"value,omitempty"`
}

// EntityMetadata is the metadata for an organization
type EntityMetadata struct {
	Version     string         `json:"version,omitempty"`
	Languages   []string       `json:"languages,omitempty"`
	Name        LanguageString `json:"name,omitempty"`
	Description LanguageString `json:"description,omitempty"`
	NewsFeed    LanguageString `json:"newsFeed,omitempty"`
	Media       EntityMedia    `json:"media,omitempty"`
	Meta        interface{}    `json:"meta,omitempty"`
	Actions     interface{}    `json:"actions,omitempty"`
}

// EntityMedia stores the avatar, header, and logo for an entity metadata
type EntityMedia struct {
	Avatar string `json:"avatar,omitempty"`
	Header string `json:"header,omitempty"`
	Logo   string `json:"logo,omitempty"`
}
