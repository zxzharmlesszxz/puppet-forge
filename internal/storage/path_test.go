package storage

import "testing"

func TestCleanObjectPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		objectPath string
		want       string
	}{
		{name: "leading slash", objectPath: "/modules/teamname/apache", want: "modules/teamname/apache"},
		{name: "relative path", objectPath: "modules/teamname/apache", want: "modules/teamname/apache"},
		{name: "cleans duplicate separators", objectPath: "/modules//teamname/../teamname/apache", want: "modules/teamname/apache"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := cleanObjectPath(tt.objectPath)
			if got != tt.want {
				t.Fatalf("cleanObjectPath(%q) = %q, want %q", tt.objectPath, got, tt.want)
			}
		})
	}
}
