// Package docker manages the lifecycle of per-user picoclaw containers via the
// Docker Engine API, spoken as raw HTTP over the daemon's unix socket (no heavy
// SDK dependency).
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ContainerState is the subset of an inspect response the manager needs.
type ContainerState struct {
	Exists  bool
	Running bool
	ID      string
	// The binds the container was CREATED with. A bind set is fixed at create
	// time — a stop/start never changes it — so this is the only way to tell that
	// a running container predates a mount it should have (see personaBindDrift).
	Binds []string
	// The RESOLVED image id the container is running, not the tag it was created
	// from. The id is what makes a harness upgrade detectable: a rebuild under the
	// same tag leaves the tag identical and changes only this (see imageDrift).
	Image string
}

// ContainerSummary is one entry from the list endpoint.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"` // "running", "exited", ...
	Labels map[string]string `json:"Labels"`
}

// CreateSpec describes a container to create.
type CreateSpec struct {
	Name    string
	Image   string
	User    string   // optional "uid:gid"; empty => image default (root)
	Cmd     []string // optional; nil => use the image's default command
	Env     []string
	Labels  map[string]string
	Binds   []string // "hostPath:containerPath[:ro]" — host paths (daemon-resolved)
	Network string
	Init    bool
}

// HTTPClient talks to the Docker daemon over its unix socket.
type HTTPClient struct {
	http *http.Client
	base string
}

// NewUnixClient returns a client bound to the daemon at socketPath
// (e.g. /var/run/docker.sock).
func NewUnixClient(socketPath string) *HTTPClient {
	return &HTTPClient{
		base: "http://docker",
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

func (c *HTTPClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker daemon unreachable: %w", err)
	}
	return resp, nil
}

func drain(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return string(bytes.TrimSpace(b))
}

// Inspect returns the container's existence/running state.
func (c *HTTPClient) Inspect(ctx context.Context, name string) (ContainerState, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return ContainerState{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		drain(resp)
		return ContainerState{Exists: false}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return ContainerState{}, fmt.Errorf("inspect %s: %s: %s", name, resp.Status, drain(resp))
	}
	var out struct {
		ID    string `json:"Id"`
		Image string `json:"Image"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		HostConfig struct {
			Binds []string `json:"Binds"`
		} `json:"HostConfig"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		resp.Body.Close()
		return ContainerState{}, err
	}
	resp.Body.Close()
	// "Image" at the TOP level is the resolved id; Config.Image is the tag the
	// container was created from. The id is deliberately the one carried: a rebuild
	// under an unchanged tag is exactly the upgrade that has to be noticed.
	return ContainerState{
		Exists: true, Running: out.State.Running, ID: out.ID,
		Binds: out.HostConfig.Binds, Image: out.Image,
	}, nil
}

// ImageID resolves an image reference to its local id, or "" when the image is not
// present locally. Deliberately does NOT pull: it answers "what would a container
// created right now run", and a reference absent locally has no answer yet.
func (c *HTTPClient) ImageID(ctx context.Context, image string) (string, error) {
	// Not PathEscape'd, for the reason EnsureImage records: Docker's
	// /images/{name:.*}/json route matches slashes literally.
	resp, err := c.do(ctx, http.MethodGet, "/images/"+image+"/json", nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusNotFound {
		drain(resp)
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("inspect image %s: %s: %s", image, resp.Status, drain(resp))
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		resp.Body.Close()
		return "", err
	}
	resp.Body.Close()
	return out.ID, nil
}

// Create creates (but does not start) a container.
func (c *HTTPClient) Create(ctx context.Context, spec CreateSpec) (string, error) {
	init := spec.Init
	body := map[string]any{
		"Image":  spec.Image,
		"Env":    spec.Env,
		"Labels": spec.Labels,
		"HostConfig": map[string]any{
			"Binds":       spec.Binds,
			"NetworkMode": spec.Network,
			"Init":        &init,
		},
	}
	if len(spec.Cmd) > 0 {
		body["Cmd"] = spec.Cmd
	}
	if spec.User != "" {
		body["User"] = spec.User
	}
	resp, err := c.do(ctx, http.MethodPost,
		"/containers/create?name="+url.QueryEscape(spec.Name), body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create %s: %s: %s", spec.Name, resp.Status, drain(resp))
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		resp.Body.Close()
		return "", err
	}
	resp.Body.Close()
	return out.ID, nil
}

// Start starts a container. Already-running (304) is not an error.
func (c *HTTPClient) Start(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		return fmt.Errorf("start %s: %s: %s", name, resp.Status, drain(resp))
	}
	drain(resp)
	return nil
}

// Stop stops a container with the given grace period. Already-stopped (304) is
// not an error.
func (c *HTTPClient) Stop(ctx context.Context, name string, grace time.Duration) error {
	secs := int(grace.Seconds())
	resp, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/containers/%s/stop?t=%d", url.PathEscape(name), secs), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		return fmt.Errorf("stop %s: %s: %s", name, resp.Status, drain(resp))
	}
	drain(resp)
	return nil
}

// Remove force-removes a container. A missing container (404) is not an error.
func (c *HTTPClient) Remove(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete,
		"/containers/"+url.PathEscape(name)+"?force=true", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("remove %s: %s: %s", name, resp.Status, drain(resp))
	}
	drain(resp)
	return nil
}

// EnsureImage pulls image if it is not already present locally. The pull is a
// streaming response; we drain it to completion. A pull failure is returned so
// the caller can decide (create would otherwise 404 with a confusing message).
func (c *HTTPClient) EnsureImage(ctx context.Context, image string) error {
	// Fast path: already present. The image name is NOT PathEscape'd — Docker's
	// /images/{name:.*}/json route matches slashes literally, so escaping them
	// to %2F would 404 and trigger a needless pull on every cold start.
	resp, err := c.do(ctx, http.MethodGet, "/images/"+image+"/json", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		drain(resp)
		return nil
	}
	drain(resp)

	fromImage, tag := splitImageTag(image)
	q := url.Values{"fromImage": {fromImage}, "tag": {tag}}
	resp, err = c.do(ctx, http.MethodPost, "/images/create?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pull %s: %s: %s", image, resp.Status, drain(resp))
	}
	// Drain the progress stream so the pull actually completes.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// splitImageTag splits "repo/name:tag" into ("repo/name", "tag"), defaulting
// the tag to "latest". Only the final path segment is checked for a tag colon,
// so a registry port (host:5000/img) is not mistaken for a tag.
func splitImageTag(image string) (string, string) {
	slash := strings.LastIndex(image, "/")
	lastSegStart := slash + 1
	if colon := strings.LastIndex(image[lastSegStart:], ":"); colon >= 0 {
		idx := lastSegStart + colon
		return image[:idx], image[idx+1:]
	}
	return image, "latest"
}

// List returns containers (including stopped) carrying the given label.
func (c *HTTPClient) List(ctx context.Context, label string) ([]ContainerSummary, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	resp, err := c.do(ctx, http.MethodGet,
		"/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list: %s: %s", resp.Status, drain(resp))
	}
	var out []ContainerSummary
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		resp.Body.Close()
		return nil, err
	}
	resp.Body.Close()
	return out, nil
}
