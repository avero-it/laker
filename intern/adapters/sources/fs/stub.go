package fs

import "github.com/avero-it/laker/pkg/config"

// Stub is a documented, disabled fs adapter for `config generate`.
func Stub() config.GenAdapter {
	return config.GenAdapter{
		Name: "documents", Type: Type, Schedule: "0 0 3 * * *",
		Comment: "Directory trees: every content stored once, every change recorded, documents searchable.",
		Params: []config.GenParam{
			{Key: "paths", Value: []string{"/home/me/Documents"}, Comment: "absolute directories; mount a NAS share and list it here"},
			{Key: "exclude", Value: []string{"**/.git/**", "**/node_modules/**", "**/.DS_Store", "**/*.tmp"}},
			{Key: "extract", Value: true, Comment: "text of PDF, Office, OpenDocument, HTML and text files for the search"},
			{Key: "extractmaxsize", Value: 52428800, Comment: "bytes"},
		},
	}
}
