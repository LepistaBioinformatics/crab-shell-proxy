package docker

import (
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// The endpoint scheme is the harness's. A ganglion container speaks HTTP
// natively -- having no WebSocket to translate is the reason it exists.
func TestEndpointAndPortFollowTheHarness(t *testing.T) {
	m := &Manager{cfg: &config.Config{
		PicoclawPort:  18790,
		GanglionPort:  18800,
		PicoclawImage: "pico:1",
		GanglionImage: "ghcr.io/x/crab-ganglion@sha256:deadbeef",
	}}

	for _, tc := range []struct {
		name     string
		agent    config.Agent
		endpoint string
		port     int
		image    string
	}{
		{
			name:     "picoclaw stays exactly as it was",
			agent:    config.Agent{Harness: config.HarnessPicoclaw},
			endpoint: "ws://c1:18790/pico/ws",
			port:     18790,
			image:    "pico:1",
		},
		{
			name:     "an empty harness is picoclaw, for back-compat",
			agent:    config.Agent{},
			endpoint: "ws://c1:18790/pico/ws",
			port:     18790,
			image:    "pico:1",
		},
		{
			name:     "ganglion is plain HTTP on its own port",
			agent:    config.Agent{Harness: config.HarnessGanglion},
			endpoint: "http://c1:18800",
			port:     18800,
			image:    "ghcr.io/x/crab-ganglion@sha256:deadbeef",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.endpoint(tc.agent, "c1"); got != tc.endpoint {
				t.Errorf("endpoint = %q, want %q", got, tc.endpoint)
			}
			if got := m.harnessPort(tc.agent); got != tc.port {
				t.Errorf("port = %d, want %d", got, tc.port)
			}
			if got := m.harnessImage(tc.agent); got != tc.image {
				t.Errorf("image = %q, want %q", got, tc.image)
			}
		})
	}
}
