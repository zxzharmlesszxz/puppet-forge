package domain

import "time"

type Module struct {
	ID            string    `json:"id"`
	Owner         string    `json:"owner"`
	Name          string    `json:"name"`
	LatestVersion string    `json:"latest_version,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type ModuleVersion struct {
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

type ReleaseMetricSummary struct {
	Source         string
	Releases       int
	LatestReleases int
}

type Release struct {
	ID              string         `json:"id"`
	ModuleID        string         `json:"module_id"`
	Owner           string         `json:"owner"`
	Name            string         `json:"name"`
	Source          string         `json:"source,omitempty"`
	Version         string         `json:"version"`
	Description     string         `json:"description,omitempty"`
	Readme          string         `json:"readme,omitempty"`
	FileName        string         `json:"file_name"`
	ContentType     string         `json:"content_type"`
	SizeBytes       int64          `json:"size_bytes"`
	SHA256          string         `json:"sha256"`
	StoragePath     string         `json:"storage_path"`
	UpstreamSlug    string         `json:"upstream_slug,omitempty"`
	UpstreamFileURI string         `json:"upstream_file_uri,omitempty"`
	DownloadURL     string         `json:"download_url,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
}

type PublishModuleInput struct {
	Owner       string
	Name        string
	Version     string
	Description string
	Readme      string
	FileName    string
	ContentType string
	FileBytes   []byte
	Metadata    map[string]any
}
