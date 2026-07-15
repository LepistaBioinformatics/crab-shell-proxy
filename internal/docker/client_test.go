package docker

import "testing"

func TestSplitImageTag(t *testing.T) {
	tests := []struct {
		in       string
		wantRepo string
		wantTag  string
	}{
		{"docker.io/sipeed/picoclaw:latest", "docker.io/sipeed/picoclaw", "latest"},
		{"sipeed/picoclaw", "sipeed/picoclaw", "latest"},
		{"picoclaw:v1.2", "picoclaw", "v1.2"},
		{"registry:5000/team/img", "registry:5000/team/img", "latest"},
		{"registry:5000/team/img:tag", "registry:5000/team/img", "tag"},
	}
	for _, tt := range tests {
		repo, tag := splitImageTag(tt.in)
		if repo != tt.wantRepo || tag != tt.wantTag {
			t.Errorf("splitImageTag(%q) = (%q,%q), want (%q,%q)",
				tt.in, repo, tag, tt.wantRepo, tt.wantTag)
		}
	}
}
