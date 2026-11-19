package model

import "encoding/json"

type Target struct {
	Alias     string `json:"alias,omitempty"`
	OrgID     string `json:"org_id"`
	ProductID string `json:"product_id"`
	VariantID string `json:"variant_id"`
	UUID      string `json:"uuid"`
	Name      string `json:"name,omitempty"`
}
type OS struct {
	Platform       string `json:"Platform"`
	MinimumVersion string `json:"MinimumVersion"`
}
type Manifest struct {
	Name       string `json:"Name"`
	UUID       string `json:"UUID"`
	Version    string `json:"Version"`
	SDKVersion int    `json:"SDKVersion"`
	Software   struct {
		MinimumVersion string `json:"MinimumVersion"`
	} `json:"Software"`
	OS []OS `json:"OS"`
}
type Package struct {
	Path     string   `json:"path"`
	SHA256   string   `json:"sha256"`
	Size     int64    `json:"size"`
	Manifest Manifest `json:"manifest"`
}
type Notes struct {
	Path            string `json:"path"`
	Markdown        string `json:"markdown"`
	PlainTextLength int    `json:"plain_text_length"`
	SHA256          string `json:"sha256"`
}
type Variant struct {
	ID   string `json:"id"`
	UUID string `json:"uuid,omitempty"`
	Name string `json:"name,omitempty"`
}
type Product struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Type        string          `json:"type,omitempty"`
	Status      string          `json:"status,omitempty"`
	Description string          `json:"description,omitempty"`
	UUID        string          `json:"uuid,omitempty"`
	Variants    []Variant       `json:"variants,omitempty"`
	Raw         json.RawMessage `json:"details,omitempty"`
}
type Version struct {
	ID           string `json:"id"`
	Version      string `json:"version"`
	Status       string `json:"status"`
	ReleaseNotes string `json:"release_notes,omitempty"`
	VariantID    string `json:"variant_id,omitempty"`
	AutoPublish  *bool  `json:"auto_publish,omitempty"`
}
type UploadResult struct {
	FileID   string   `json:"file_id"`
	FolderID string   `json:"folder_id"`
	Manifest Manifest `json:"manifest"`
}
type Submission struct {
	Package     Package      `json:"package"`
	Notes       Notes        `json:"notes"`
	AutoPublish bool         `json:"auto_publish"`
	Upload      UploadResult `json:"upload"`
}
type SubmitResult struct {
	ReceiptID string `json:"receipt_id,omitempty"`
	ID        string `json:"id"`
	Message   string `json:"message,omitempty"`
}
