package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
	"github.com/klauspost/compress/zstd"
)

// Fixed UUIDs used across the authorization table tests.
const (
	accAlice = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	accBob   = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	tenantT  = "11111111-1111-1111-1111-111111111111"
	subsX    = "22222222-2222-2222-2222-222222222222"
	tenantU  = "33333333-3333-3333-3333-333333333333"
)

type fakeOrch struct {
	projects   []projects.Project
	projectErr error
	ensureErr  error
	armed      int
	scaffolded map[string]bool
	keys       []docker.WorkspaceKey

	writeErr   error
	deleteErr  error
	writes     []secretWrite
	deletes    []secretWrite
	restarts   []docker.WorkspaceKey
	listResult docker.SecretNames
	memory     string

	// admin-bulk-instance-config recording + canned results.
	bulkInspect      docker.ScopeConfigInspection
	bulkInspectErr   error
	bulkInspectKeys  []string
	bulkInspectScope []docker.Scope
	bulkApplied      []docker.ScopeConfigChange
	bulkResult       docker.ScopeConfigResult
	bulkApplyErr     error
	bulkCatalog      docker.TemplateCatalog
	bulkCatalogErr   error
	bulkCatalogNames []string
	bulkTemplate     docker.TemplateResult
	bulkTemplateErr  error
	bulkTemplateArgs []bulkTemplateCall
	bulkOverlay      docker.OverlayResult
	bulkOverlayErr   error
	bulkOverlayArgs  []bulkOverlayCall

	// admin-shared-content recording + canned results.
	sharedFiles   []docker.FileMeta
	userFiles     []docker.FileMeta
	users         []docker.UserRef
	tenants       []string
	tenantSubs    []string
	sharedWrites  []docker.Scope
	sharedDeletes []docker.Scope
	// Persona writes record the CONTENT, not just the scope: the fields arriving
	// empty is precisely how the multipart-parse bug presented, so a test has to
	// be able to assert what actually landed.
	personaWrites []personaWrite
	// Names the harness delivered as attachments, so a test can assert the file
	// actually reached the workspace.
	attachmentWrites []string
	// Canned answer for the resolved persona read (content + which layer produced
	// it), so a handler test can exercise the inherited-preload path.
	personaDoc       string
	personaDocSource string
	personaDocErr    error
	nativeUnsets     []string
	userFileDeletes  []docker.WorkspaceKey

	// model re-apply fakes: record calls, return canned results.
	reapplyScopes    []docker.Scope
	reapplyUserKeys  []docker.WorkspaceKey
	reapplyForModels []string
	// reapplyCalls counts every ReapplyModel* call, so a test can assert a
	// no-op operation (e.g. reorder) triggered none at all.
	reapplyCalls int

	// reg backs SetModelAssignment/ClearModelAssignment, mirroring what the real
	// Manager does against the registry.
	reg *registry.Registry

	// restart-control recording. The notice store is real (over a temp dir) so
	// the derived pending/not-pending rules are exercised rather than faked;
	// only the container side is recorded.
	restartStore     *restart.Store
	propagatedScopes []docker.Scope
	bouncedScopes    []docker.Scope
	armedSchedules   []docker.Scope
	workspaceNotices []docker.WorkspaceKey
	statusRunning    bool
	restartErr       error

	// admin-instance-config-editor recording + canned results.
	instanceConfig           docker.InstanceConfig
	instanceConfigAfterWrite docker.InstanceConfig
	instanceConfigReapply    docker.ReapplyResult
	instanceConfigErr        error
	instanceConfigWriteErr   error
	instanceConfigReadKeys   []docker.WorkspaceKey
	instanceConfigWriteKey   docker.WorkspaceKey
	instanceConfigWritten    string
	instanceConfigRevision   string

	// Uploads-tree organisation recording + canned results.
	folderCreated []string
	folderDeleted []string
	moved         []string
	removedFiles  int
	folderErr     error
}

type personaWrite struct {
	scope      docker.Scope
	name, body string
}

type secretWrite struct {
	key                 docker.WorkspaceKey
	format, name, value string
}

func newFakeOrch() *fakeOrch { return &fakeOrch{scaffolded: map[string]bool{}} }

// restarts_ is a REAL notice store over a per-fake temp dir, so the derived
// pending/not-pending rules are exercised rather than faked. It must never fall
// back to a shared directory: two tests sharing one root leak notices into each
// other and a "no notice was raised" assertion silently passes on someone else's
// state.
func (f *fakeOrch) restarts_() *restart.Store {
	if f.restartStore == nil {
		dir, err := os.MkdirTemp("", "fakeorch-restart-")
		if err != nil {
			panic("fakeOrch: temp restart root: " + err.Error())
		}
		f.restartStore = restart.NewStore(dir)
	}
	return f.restartStore
}

func (f *fakeOrch) RestartStatus(key docker.WorkspaceKey) (docker.RestartStatus, error) {
	st, err := f.restarts_().Status(key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err != nil {
		return docker.RestartStatus{}, err
	}
	return docker.RestartStatus{Status: st, Running: f.statusRunning}, nil
}

func (f *fakeOrch) RaiseWorkspaceRestartNotice(key docker.WorkspaceKey, reason restart.Reason) error {
	f.workspaceNotices = append(f.workspaceNotices, key)
	return f.restarts_().RaiseWorkspace(key.TenantID, key.SubsAccID, key.Role, key.UserAccID, reason, time.Now().UTC())
}

func (f *fakeOrch) RaiseRestartNotice(scope docker.Scope, n restart.Notice) error {
	return f.restarts_().Raise(scope.TenantID, scope.SubsAccID, scope.AgentKey, n)
}

func (f *fakeOrch) RestartNotice(scope docker.Scope) (restart.Notice, bool, error) {
	return f.restarts_().Get(scope.TenantID, scope.SubsAccID, scope.AgentKey)
}

func (f *fakeOrch) WithdrawRestartNotice(scope docker.Scope) error {
	return f.restarts_().Withdraw(scope.TenantID, scope.SubsAccID, scope.AgentKey)
}

func (f *fakeOrch) PropagateScope(scope docker.Scope) error {
	f.propagatedScopes = append(f.propagatedScopes, scope)
	return nil
}

func (f *fakeOrch) BounceScope(scope docker.Scope) error {
	f.bouncedScopes = append(f.bouncedScopes, scope)
	return nil
}

func (f *fakeOrch) ArmScheduledBounce(scope docker.Scope, _ time.Time) {
	f.armedSchedules = append(f.armedSchedules, scope)
}

func (f *fakeOrch) WriteSecret(_ config.Agent, key docker.WorkspaceKey, format, name, value string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, secretWrite{key, format, name, value})
	return nil
}

func (f *fakeOrch) ListSecrets(docker.WorkspaceKey) (docker.SecretNames, error) {
	return f.listResult, nil
}

func (f *fakeOrch) ListSharedSkills(docker.Scope) ([]docker.SkillMeta, error) { return nil, nil }
func (f *fakeOrch) ReadSharedSkillDoc(docker.Scope, string) (string, docker.SkillMeta, error) {
	return "", docker.SkillMeta{}, nil
}
func (f *fakeOrch) WriteSharedSkillDoc(docker.Scope, string, string) error    { return nil }
func (f *fakeOrch) WriteSharedSkillZip(docker.Scope, string, io.Reader) error { return nil }
func (f *fakeOrch) ArchiveSharedSkill(docker.Scope, string, io.Writer) error  { return nil }
func (f *fakeOrch) DeleteSharedSkill(docker.Scope, string) error              { return nil }
func (f *fakeOrch) SyncEffectiveSkillsForScope(docker.Scope) error            { return nil }

func (f *fakeOrch) ListPersona(docker.Scope) ([]docker.PersonaEntry, error) {
	return nil, nil
}
func (f *fakeOrch) ReadPersona(docker.Scope, string) (string, string, error) {
	return f.personaDoc, f.personaDocSource, f.personaDocErr
}
func (f *fakeOrch) WritePersona(scope docker.Scope, name, body string) error {
	f.personaWrites = append(f.personaWrites, personaWrite{scope: scope, name: name, body: body})
	return nil
}
func (f *fakeOrch) DeletePersona(docker.Scope, string) error        { return nil }
func (f *fakeOrch) SyncEffectivePersonaForScope(docker.Scope) error { return nil }

func (f *fakeOrch) DeleteSecret(key docker.WorkspaceKey, format, name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes = append(f.deletes, secretWrite{key: key, format: format, name: name})
	return nil
}

func (f *fakeOrch) RestartWorkspace(key docker.WorkspaceKey) error {
	if f.restartErr != nil {
		return f.restartErr
	}
	f.restarts = append(f.restarts, key)
	return f.restarts_().Stamp(key.TenantID, key.SubsAccID, key.Role, key.UserAccID, time.Now().UTC())
}

func (f *fakeOrch) StoreMedia(_ docker.WorkspaceKey, _, rawName string, r io.Reader) (docker.StoredMedia, error) {
	n, _ := io.Copy(io.Discard, r)
	return docker.StoredMedia{Path: "uploads/test-" + rawName, Name: rawName, Size: n}, nil
}

func (f *fakeOrch) StoreAgentAttachment(_ docker.WorkspaceKey, _, rawName string, r io.Reader) (docker.StoredMedia, error) {
	n, _ := io.Copy(io.Discard, r)
	f.attachmentWrites = append(f.attachmentWrites, rawName)
	return docker.StoredMedia{
		Path: "uploads/attachments/" + rawName, Name: "attachments/" + rawName, Size: n,
	}, nil
}

func (f *fakeOrch) ListMedia(docker.WorkspaceKey, string) ([]docker.StoredMedia, error) {
	return nil, nil
}

func (f *fakeOrch) DeleteMedia(docker.WorkspaceKey, string, string) error {
	return nil
}

// Folder operations record what they were asked to do, so a handler test can assert
// the path actually forwarded rather than only the status code — the mediaRelPath
// stripping is exactly the kind of thing a status assertion would miss.
func (f *fakeOrch) CreateFolder(_ docker.WorkspaceKey, _, rel string) error {
	f.folderCreated = append(f.folderCreated, rel)
	return f.folderErr
}

func (f *fakeOrch) MoveMedia(_ docker.WorkspaceKey, _, fromRel, toRel string) error {
	f.moved = append(f.moved, fromRel+" -> "+toRel)
	return f.folderErr
}

func (f *fakeOrch) DeleteFolder(_ docker.WorkspaceKey, _, rel string) (int, error) {
	f.folderDeleted = append(f.folderDeleted, rel)
	return f.removedFiles, f.folderErr
}

func (f *fakeOrch) OpenMedia(docker.WorkspaceKey, string, string) (io.ReadCloser, string, error) {
	return io.NopCloser(strings.NewReader("data")), "file.txt", nil
}

func (f *fakeOrch) ReadMemory(docker.WorkspaceKey, string) (string, error) {
	return f.memory, nil
}

func (f *fakeOrch) WriteMemory(_ docker.WorkspaceKey, _, content string) error {
	f.memory = content
	return nil
}

// --- admin-instance-config-editor fakes ---

func (f *fakeOrch) ReadInstanceConfig(key docker.WorkspaceKey) (docker.InstanceConfig, error) {
	f.instanceConfigReadKeys = append(f.instanceConfigReadKeys, key)
	if f.instanceConfigErr != nil {
		return docker.InstanceConfig{}, f.instanceConfigErr
	}
	return f.instanceConfig, nil
}

func (f *fakeOrch) WriteInstanceConfig(key docker.WorkspaceKey, raw, revision string) (docker.InstanceConfig, docker.ReapplyResult, error) {
	f.instanceConfigWriteKey = key
	f.instanceConfigWritten = raw
	f.instanceConfigRevision = revision
	if f.instanceConfigWriteErr != nil {
		return docker.InstanceConfig{}, docker.ReapplyResult{}, f.instanceConfigWriteErr
	}
	return f.instanceConfigAfterWrite, f.instanceConfigReapply, nil
}

// --- admin-bulk-instance-config fakes ---

type bulkOverlayCall struct {
	Scope docker.Scope
	Key   string
	Value string
	By    string
}

type bulkTemplateCall struct {
	Template string
	Key      string
	Value    any
	Revision string
	By       string
}

func (f *fakeOrch) InspectScopeConfigKey(scope docker.Scope, key string) (docker.ScopeConfigInspection, error) {
	f.bulkInspectKeys = append(f.bulkInspectKeys, key)
	f.bulkInspectScope = append(f.bulkInspectScope, scope)
	if f.bulkInspectErr != nil {
		return docker.ScopeConfigInspection{}, f.bulkInspectErr
	}
	return f.bulkInspect, nil
}

func (f *fakeOrch) ApplyScopeConfigKey(scope docker.Scope, ch docker.ScopeConfigChange) (docker.ScopeConfigResult, error) {
	f.bulkApplied = append(f.bulkApplied, ch)
	if f.bulkApplyErr != nil {
		return docker.ScopeConfigResult{}, f.bulkApplyErr
	}
	return f.bulkResult, nil
}

func (f *fakeOrch) TemplateConfigKeys(template string) (docker.TemplateCatalog, error) {
	f.bulkCatalogNames = append(f.bulkCatalogNames, template)
	if f.bulkCatalogErr != nil {
		return docker.TemplateCatalog{}, f.bulkCatalogErr
	}
	return f.bulkCatalog, nil
}

func (f *fakeOrch) ApplyOverlayConfigKey(scope docker.Scope, key string, value json.RawMessage, by string, at time.Time) (docker.OverlayResult, error) {
	f.bulkOverlayArgs = append(f.bulkOverlayArgs, bulkOverlayCall{scope, key, string(value), by})
	if f.bulkOverlayErr != nil {
		return docker.OverlayResult{}, f.bulkOverlayErr
	}
	return f.bulkOverlay, nil
}

func (f *fakeOrch) ApplyTemplateConfigKey(template, key string, value any, revision, by string, at time.Time) (docker.TemplateResult, error) {
	f.bulkTemplateArgs = append(f.bulkTemplateArgs, bulkTemplateCall{template, key, value, revision, by})
	if f.bulkTemplateErr != nil {
		return docker.TemplateResult{}, f.bulkTemplateErr
	}
	return f.bulkTemplate, nil
}

// --- admin-shared-content fakes: record calls, return canned results ---

func (f *fakeOrch) ListSharedFiles(docker.Scope) ([]docker.FileMeta, error) {
	return f.sharedFiles, nil
}

func (f *fakeOrch) WriteSharedFile(scope docker.Scope, rawName string, r io.Reader) (docker.StoredMedia, error) {
	n, _ := io.Copy(io.Discard, r)
	f.sharedWrites = append(f.sharedWrites, scope)
	return docker.StoredMedia{Path: rawName, Name: rawName, Size: n}, nil
}

func (f *fakeOrch) ReadSharedFile(_ docker.Scope, name string) (io.ReadCloser, docker.FileMeta, error) {
	return io.NopCloser(strings.NewReader("bytes")), docker.FileMeta{Name: name, Size: 5}, nil
}

func (f *fakeOrch) DeleteSharedFile(scope docker.Scope, _ string) error {
	f.sharedDeletes = append(f.sharedDeletes, scope)
	return nil
}

func (f *fakeOrch) WriteSharedSecret(scope docker.Scope, format, _, _ string) error {
	if format != docker.FormatDotenv && format != docker.FormatJSON {
		return docker.ErrInvalidSecretName
	}
	f.sharedWrites = append(f.sharedWrites, scope)
	return nil
}

func (f *fakeOrch) ListSharedSecrets(docker.Scope) (docker.SecretNames, error) {
	return f.listResult, nil
}

func (f *fakeOrch) DeleteSharedSecret(scope docker.Scope, _, _ string) error {
	f.sharedDeletes = append(f.sharedDeletes, scope)
	return nil
}

func (f *fakeOrch) UnsetNativeSlotForScope(scope docker.Scope, slot string) {
	f.nativeUnsets = append(f.nativeUnsets, scope.AgentKey+"|"+slot)
}

func (f *fakeOrch) ListTenants() ([]string, error) {
	return f.tenants, nil
}

func (f *fakeOrch) ListTenantSubscriptions(_ string) ([]string, error) {
	return f.tenantSubs, nil
}

func (f *fakeOrch) ListSubscriptionUsers(_, _ string) ([]docker.UserRef, error) {
	return f.users, nil
}

func (f *fakeOrch) ListUserFiles(docker.WorkspaceKey, string) ([]docker.FileMeta, error) {
	return f.userFiles, nil
}

func (f *fakeOrch) DeleteUserFile(key docker.WorkspaceKey, _, _ string) error {
	f.userFileDeletes = append(f.userFileDeletes, key)
	return nil
}

func (f *fakeOrch) ReapplyModelScope(scope docker.Scope, _ bool) error {
	f.reapplyCalls++
	f.reapplyScopes = append(f.reapplyScopes, scope)
	return nil
}

func (f *fakeOrch) ReapplyModelUser(key docker.WorkspaceKey, _ bool) error {
	f.reapplyCalls++
	f.reapplyUserKeys = append(f.reapplyUserKeys, key)
	return nil
}

func (f *fakeOrch) ReapplyModelForModel(modelName string, _ bool) error {
	f.reapplyCalls++
	f.reapplyForModels = append(f.reapplyForModels, modelName)
	return nil
}

func (f *fakeOrch) SetModelAssignment(key docker.WorkspaceKey, modelName string, _ bool) error {
	return f.reg.PutAssignment(registry.WorkspaceRef{
		TenantID: key.TenantID, SubsAccID: key.SubsAccID, Agent: key.Role, UserAccID: key.UserAccID,
	}, registry.Assignment{ModelName: modelName, Source: registry.SourceExplicit})
}

func (f *fakeOrch) ClearModelAssignment(key docker.WorkspaceKey, _ bool) error {
	return f.reg.DeleteAssignment(registry.WorkspaceRef{
		TenantID: key.TenantID, SubsAccID: key.SubsAccID, Agent: key.Role, UserAccID: key.UserAccID,
	})
}

func skey(tenantID, subsAccID string) string { return tenantID + "/" + subsAccID }

func (f *fakeOrch) EnsureRunning(_ context.Context, _ config.Agent, key docker.WorkspaceKey, _ string) (docker.Target, error) {
	if f.ensureErr != nil {
		return docker.Target{}, f.ensureErr
	}
	f.keys = append(f.keys, key)
	return docker.Target{Name: "picoclaw-alpha-h", Endpoint: "ws://x:1/pico/ws", AuthToken: "t"}, nil
}
func (f *fakeOrch) ArmIdle(config.Agent, docker.WorkspaceKey) { f.armed++ }
func (f *fakeOrch) ScaffoldSubscription(tenantID, subsAccID string) (bool, error) {
	k := skey(tenantID, subsAccID)
	if f.scaffolded[k] {
		return false, nil
	}
	f.scaffolded[k] = true
	return true, nil
}
func (f *fakeOrch) SubscriptionScaffolded(tenantID, subsAccID string) bool {
	return f.scaffolded[skey(tenantID, subsAccID)]
}

type fakeTurner struct {
	content string
	err     error
	deltas  []string
}

func (f *fakeTurner) RunTurn(_ context.Context, _ turn.Request, sink turn.Sink) (string, error) {
	for _, d := range f.deltas {
		sink.EmitContent(d)
	}
	return f.content, f.err
}

func encodeProfile(t *testing.T, body string) string {
	t.Helper()
	enc, _ := zstd.NewWriter(nil)
	out := enc.EncodeAll([]byte(body), nil)
	_ = enc.Close()
	return base64.StdEncoding.EncodeToString(out)
}

// licensedProfile builds a profile JSON for accId with a single licensed
// resource record {accId=subsAccID, tenantId, role, perm, verified}.
func licensedProfile(accID, tenantID, subsAccID, role, perm string, verified bool) string {
	v := "false"
	if verified {
		v = "true"
	}
	return `{"accId":"` + accID + `","owners":[{"email":"u@x","isPrincipal":true}],` +
		`"licensedResources":{"records":[{"accId":"` + subsAccID + `","tenantId":"` + tenantID +
		`","role":"` + role + `","perm":"` + perm + `","verified":` + v + `}]}}`
}

func testServer(orch Orchestrator, turner Turner) *Server {
	res := identity.NewSDKResolver()
	cfg := &config.Config{
		ContainerDataRoot: "/tmp",
		Agents: map[string]config.Agent{
			"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
				Mode: config.ModeScaleToZero},
		},
	}
	return &Server{Cfg: cfg, Resolver: res, Mgr: orch, Pico: turner}
}

func TestOpenAPIDocServedUnauthenticated(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	// No Authorization / profile headers: mycelium discovery fetches this
	// directly from the service host, unauthenticated.
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/doc/openapi.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Paths   map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi version = %q, want 3.x", doc.OpenAPI)
	}
	if doc.Paths["/v1/chat/completions"]["post"].OperationID != "chatCompletion" {
		t.Errorf("chatCompletion operation missing; paths = %+v", doc.Paths)
	}
}

func chatReq(t *testing.T, body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func headersFor(t *testing.T, profileJSON string) map[string]string {
	return map[string]string{
		identity.ServiceNameHeader: "picoclaw-alpha",
		"Authorization":            "Bearer bearer",
		identity.ProfileHeader:     encodeProfile(t, profileJSON),
	}
}

// goodHeaders carries alice, licensed (write, verified, role alpha) into subsX
// under tenantT.
func goodHeaders(t *testing.T) map[string]string {
	return headersFor(t, licensedProfile(accAlice, tenantT, subsX, "alpha", "write", true))
}

// goodBody is an authorized chat body targeting tenantT / subsX.
const goodBody = `{"messages":[{"role":"user","content":"hi"}],"session_id":"s","tenant_id":"` +
	tenantT + `","subs_acc_id":"` + subsX + `"}`

// scaffoldedOrch returns a fakeOrch with tenantT/subsX already scaffolded.
func scaffoldedOrch() *fakeOrch {
	o := newFakeOrch()
	o.scaffolded[skey(tenantT, subsX)] = true
	return o
}

func TestChatUnknownService(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, `{}`, nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestChatBadToken(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	h := goodHeaders(t)
	h["Authorization"] = "Bearer wrong"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, h))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestChatNoProfile(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	h := goodHeaders(t)
	delete(h, identity.ProfileHeader)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, h))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestChatNoSessionID(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	body := `{"messages":[{"role":"user","content":"hi"}],"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, body, goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestChatMissingTenantOrSubs(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	// Missing tenant_id.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t,
		`{"messages":[{"role":"user","content":"hi"}],"session_id":"s","subs_acc_id":"`+subsX+`"}`, goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing tenant_id: status = %d, want 400", w.Code)
	}
	// Missing subs_acc_id.
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t,
		`{"messages":[{"role":"user","content":"hi"}],"session_id":"s","tenant_id":"`+tenantT+`"}`, goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing subs_acc_id: status = %d, want 400", w.Code)
	}
}

func TestChatOKJSON(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{content: "hello back"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello back") {
		t.Errorf("body missing content: %s", w.Body.String())
	}
	if orch.armed != 1 {
		t.Errorf("ArmIdle called %d times, want 1", orch.armed)
	}
	// Routed to alice's own leaf under the targeted subscription.
	if len(orch.keys) != 1 || orch.keys[0].UserAccID != accAlice ||
		orch.keys[0].SubsAccID != subsX || orch.keys[0].TenantID != tenantT || orch.keys[0].Role != "alpha" {
		t.Errorf("routed key = %+v", orch.keys)
	}
}

func TestChatScaffoldsOnDemand(t *testing.T) {
	// Authorized chat against a not-yet-scaffolded subscription: the root is
	// created on demand (no 409), then the turn runs.
	orch := newFakeOrch()
	s := testServer(orch, &fakeTurner{content: "hi"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !orch.SubscriptionScaffolded(tenantT, subsX) {
		t.Error("subscription was not scaffolded on demand")
	}
}

func TestChatForbiddenDenyPaths(t *testing.T) {
	cases := []struct {
		name    string
		profile string
	}{
		{"unlicensed", `{"accId":"` + accAlice + `","owners":[{"email":"u@x","isPrincipal":true}]}`},
		{"read-only", licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true)},
		{"wrong-tenant", licensedProfile(accAlice, tenantU, subsX, "alpha", "write", true)},
		{"missing-role", licensedProfile(accAlice, tenantT, subsX, "beta", "write", true)},
		{"acc-equals-subs", licensedProfile(subsX, tenantT, subsX, "alpha", "write", true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(scaffoldedOrch(), &fakeTurner{content: "x"})
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, chatReq(t, goodBody, headersFor(t, tc.profile)))
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (%s): %s", w.Code, tc.name, w.Body.String())
			}
		})
	}
}

// TestChatUnverifiedAccepted locks the user decision (2026-07-16) that `verified`
// is NOT enforced: an otherwise-valid grant (write, right tenant/role/account)
// that is unverified still authorizes the chat.
func TestChatUnverifiedAccepted(t *testing.T) {
	profile := licensedProfile(accAlice, tenantT, subsX, "alpha", "write", false)
	s := testServer(scaffoldedOrch(), &fakeTurner{content: "ok"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, headersFor(t, profile)))
	if w.Code != http.StatusOK {
		t.Errorf("unverified grant: status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestChatStaffShortCircuit(t *testing.T) {
	// A staff profile with NO licensed resources still passes the chain.
	profile := `{"accId":"` + accAlice + `","isStaff":true,"owners":[{"email":"u@x","isPrincipal":true}]}`
	s := testServer(scaffoldedOrch(), &fakeTurner{content: "ok"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, headersFor(t, profile)))
	if w.Code != http.StatusOK {
		t.Errorf("staff short-circuit: status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestChatTwoMembersIsolate(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{content: "x"})
	// Alice.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("alice status = %d", w.Code)
	}
	// Bob, licensed into the same subscription.
	bob := headersFor(t, licensedProfile(accBob, tenantT, subsX, "alpha", "write", true))
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, bob))
	if w.Code != http.StatusOK {
		t.Fatalf("bob status = %d", w.Code)
	}
	if len(orch.keys) != 2 {
		t.Fatalf("want 2 routed keys, got %d", len(orch.keys))
	}
	if orch.keys[0].UserAccID == orch.keys[1].UserAccID {
		t.Errorf("two members collapsed to the same user dir: %v", orch.keys)
	}
	if orch.keys[0].UserAccID != accAlice || orch.keys[1].UserAccID != accBob {
		t.Errorf("routed keys = %+v", orch.keys)
	}
}

func TestChatManagerErrorIs502(t *testing.T) {
	orch := scaffoldedOrch()
	orch.ensureErr = context.DeadlineExceeded
	s := testServer(orch, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, goodHeaders(t)))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestChatStreamFraming(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{deltas: []string{"Hel", "lo"}})
	body := `{"messages":[{"role":"user","content":"hi"}],"session_id":"s","stream":true,"tenant_id":"` +
		tenantT + `","subs_acc_id":"` + subsX + `"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, body, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	out := w.Body.String()
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Error("missing initial role chunk")
	}
	if !strings.Contains(out, `"content":"Hel"`) || !strings.Contains(out, `"content":"lo"`) {
		t.Errorf("missing streamed deltas: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Error("missing [DONE] terminator")
	}
}

// realMgrServer builds a Server backed by a real *docker.Manager over a temp
// data root, for the pure-filesystem scaffold endpoints. The Docker handle is
// nil because these endpoints never touch it.
func realMgrServer(t *testing.T, webhookSecret string) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		ContainerDataRoot: root, ResolvedWebhookSecret: webhookSecret,
		Agents: map[string]config.Agent{
			"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
				Mode: config.ModeScaleToZero},
		},
	}
	mgr := docker.NewManager(cfg, nil, func(context.Context, string, int) error { return nil }, nil, nil)
	return &Server{Cfg: cfg, Resolver: identity.NewSDKResolver(), Mgr: mgr}, root
}

const accountBody = `{"id":"` + subsX + `","accountType":{"subscription":{"tenantId":"` + tenantT + `"}}}`

func accountReq(t *testing.T, body, auth string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/accounts", strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

func TestAccountsScaffold201Then200(t *testing.T) {
	s, root := realMgrServer(t, "wh-secret")
	// First create -> 201 and the scaffold dir appears.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, accountReq(t, accountBody, "Bearer wh-secret"))
	if w.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(config.SubscriptionRoot(root, tenantT, subsX)); err != nil {
		t.Fatalf("scaffold dir not present: %v", err)
	}
	// Retry -> 200 (idempotent).
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, accountReq(t, accountBody, "Bearer wh-secret"))
	if w.Code != http.StatusOK {
		t.Errorf("retry status = %d, want 200", w.Code)
	}
}

func TestAccountsBadSecret401(t *testing.T) {
	s, root := realMgrServer(t, "wh-secret")
	for _, auth := range []string{"", "Bearer wrong"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, accountReq(t, accountBody, auth))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: status = %d, want 401", auth, w.Code)
		}
	}
	if _, err := os.Stat(config.SubscriptionRoot(root, tenantT, subsX)); err == nil {
		t.Error("scaffold created despite bad secret")
	}
}

func TestAccountsEmptyConfiguredSecretRejects(t *testing.T) {
	s, _ := realMgrServer(t, "") // no secret configured
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, accountReq(t, accountBody, "Bearer "))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 when no secret configured", w.Code)
	}
}

func TestAccountsBadBody400(t *testing.T) {
	s, _ := realMgrServer(t, "wh-secret")
	cases := []string{
		`{"accountType":{"subscription":{"tenantId":"` + tenantT + `"}}}`, // missing id
		`{"id":"` + subsX + `","accountType":{"user":{}}}`,                // not a subscription
		`{"id":"not-a-uuid","accountType":{"subscription":{"tenantId":"` + tenantT + `"}}}`,
		`{"id":"` + subsX + `","accountType":{"subscription":{"tenantId":"nope"}}}`,
	}
	for _, body := range cases {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, accountReq(t, body, "Bearer wh-secret"))
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, w.Code)
		}
	}
}

func subsReq(t *testing.T, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/subscriptions", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestSubscriptionsList(t *testing.T) {
	s, _ := realMgrServer(t, "wh-secret")
	h := map[string]string{identity.ProfileHeader: encodeProfile(t,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "write", true))}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, subsReq(t, h))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, subsX) || !strings.Contains(body, tenantT) ||
		!strings.Contains(body, `"role":"alpha"`) || !strings.Contains(body, `"perm":"write"`) {
		t.Errorf("subscription tuple missing: %s", body)
	}
	if !strings.Contains(body, `"scaffolded":false`) {
		t.Errorf("expected scaffolded=false annotation: %s", body)
	}
}

func TestSubscriptionsEmpty(t *testing.T) {
	s, _ := realMgrServer(t, "wh-secret")
	h := map[string]string{identity.ProfileHeader: encodeProfile(t,
		`{"accId":"`+accAlice+`","owners":[{"email":"u@x","isPrincipal":true}]}`)}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, subsReq(t, h))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"subscriptions":[]`) {
		t.Errorf("expected empty list: %s", w.Body.String())
	}
}

func TestSubscriptionsNoProfile401(t *testing.T) {
	s, _ := realMgrServer(t, "wh-secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, subsReq(t, nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func historyReq(t *testing.T, query string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/sessions/history?"+query, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestHistoryOKNewPath(t *testing.T) {
	// A read from the new layout for the caller's own leaf; empty (no dir yet)
	// but 200 with a messages field, proving the path resolved without error.
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	q := "session_id=s&tenant_id=" + tenantT + "&subs_acc_id=" + subsX
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, historyReq(t, q, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"messages"`) {
		t.Errorf("missing messages field: %s", w.Body.String())
	}
}

func TestHistoryRequiresTenantAndSubs(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	for _, q := range []string{
		"tenant_id=" + tenantT + "&subs_acc_id=" + subsX, // missing session_id
		"session_id=s&subs_acc_id=" + subsX,              // missing tenant_id
		"session_id=s&tenant_id=" + tenantT,              // missing subs_acc_id
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, historyReq(t, q, goodHeaders(t)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d, want 400", q, w.Code)
		}
	}
}

func TestHistoryAccountSwitchGuard(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	// profile accId == subs_acc_id -> 403.
	h := headersFor(t, licensedProfile(subsX, tenantT, subsX, "alpha", "write", true))
	q := "session_id=s&tenant_id=" + tenantT + "&subs_acc_id=" + subsX
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, historyReq(t, q, h))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestModelsRequiresAuth(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	// no headers -> unknown service
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	// good headers -> 200 list
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	for k, v := range goodHeaders(t) {
		r.Header.Set(k, v)
	}
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "picoclaw") {
		t.Errorf("models: status=%d body=%s", w.Code, w.Body.String())
	}
}

func secretBody(format, name, value string) string {
	return `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","format":"` + format + `","name":"` + name + `","value":"` + value + `"}`
}

func secretsPostReq(t *testing.T, body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/secrets", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func secretsReq(t *testing.T, method, query string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, "/v1/secrets?"+query, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestSecretsPostEachFormat(t *testing.T) {
	// native is deliberately absent: it moved to the admin surface
	// (native-secrets-admin-only AC-1, asserted by TestSecretsNativeRejected).
	for _, format := range []string{"dotenv", "json", "file"} {
		t.Run(format, func(t *testing.T) {
			orch := scaffoldedOrch()
			s := testServer(orch, &fakeTurner{})
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody(format, "web.brave", "sekret"), goodHeaders(t)))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			if len(orch.writes) != 1 || orch.writes[0].format != format {
				t.Errorf("write not recorded: %+v", orch.writes)
			}
			// Routed to alice's own (user, agent) store, not the subscription.
			if orch.writes[0].key.UserAccID != accAlice || orch.writes[0].key.Role != "alpha" ||
				orch.writes[0].key.SubsAccID != subsX || orch.writes[0].key.TenantID != tenantT {
				t.Errorf("routed key = %+v", orch.writes[0].key)
			}
			// DEC-3: a member's own secret write no longer force-restarts them
			// mid-conversation. It leaves a notice on their own marker; they press
			// the button.
			if len(orch.restarts) != 0 {
				t.Errorf("container bounced %d times, want 0 (the member decides when)", len(orch.restarts))
			}
			if len(orch.workspaceNotices) != 1 {
				t.Errorf("workspace notices = %d, want 1", len(orch.workspaceNotices))
			}
		})
	}
}

func TestSecretsPostValidationMapsTo400(t *testing.T) {
	for _, sentinel := range []error{docker.ErrInvalidSecretName, docker.ErrUnknownNativeSlot} {
		orch := scaffoldedOrch()
		orch.writeErr = sentinel
		s := testServer(orch, &fakeTurner{})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody("dotenv", "BAD NAME", "v"), goodHeaders(t)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("sentinel %v: status = %d, want 400", sentinel, w.Code)
		}
		if len(orch.restarts) != 0 {
			t.Errorf("sentinel %v: restart must not run when write is rejected", sentinel)
		}
	}
}

// TestSecretsNativeRejected proves native-secrets-admin-only AC-1: the per-user
// endpoint refuses to WRITE the native format, in the PROXY (not only the webapp
// BFF), and nothing is written or restarted. The caller here is a normal chat
// user; the gate is unconditional, so tier does not matter. Deleting a pre-gate
// entry stays allowed — it is the user's own data and cannot inject a credential.
func TestSecretsNativeRejected(t *testing.T) {
	t.Run("post", func(t *testing.T) {
		orch := scaffoldedOrch()
		s := testServer(orch, &fakeTurner{})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody("native", "web.brave", "sekret"), goodHeaders(t)))
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
		}
		if len(orch.writes) != 0 {
			t.Errorf("native write reached the manager: %+v", orch.writes)
		}
		if len(orch.restarts) != 0 {
			t.Errorf("restart must not run for a rejected native write")
		}
	})
	t.Run("delete of a legacy entry stays allowed", func(t *testing.T) {
		orch := scaffoldedOrch()
		s := testServer(orch, &fakeTurner{})
		w := httptest.NewRecorder()
		url := "/v1/secrets?format=native&name=web.brave&tenant_id=" + tenantT + "&subs_acc_id=" + subsX
		req := httptest.NewRequest(http.MethodDelete, url, nil)
		for k, v := range goodHeaders(t) {
			req.Header.Set(k, v)
		}
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
	})
}

func TestSecretsPostUnknownFormat400(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody("yaml", "X", "v"), goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown format", w.Code)
	}
	if len(orch.writes) != 0 {
		t.Error("write ran for an unknown format")
	}
}

func TestSecretsPostNoProfile401(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	h := goodHeaders(t)
	delete(h, identity.ProfileHeader)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody("dotenv", "A", "v"), h))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestSecretsPostForbidden(t *testing.T) {
	cases := []struct{ name, profile string }{
		{"unlicensed", `{"accId":"` + accAlice + `","owners":[{"email":"u@x","isPrincipal":true}]}`},
		{"read-only", licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true)},
		{"wrong-tenant", licensedProfile(accAlice, tenantU, subsX, "alpha", "write", true)},
		{"missing-role", licensedProfile(accAlice, tenantT, subsX, "beta", "write", true)},
		{"acc-equals-subs", licensedProfile(subsX, tenantT, subsX, "alpha", "write", true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := scaffoldedOrch()
			s := testServer(orch, &fakeTurner{})
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody("dotenv", "A", "v"), headersFor(t, tc.profile)))
			if w.Code != http.StatusForbidden {
				t.Errorf("%s: status = %d, want 403", tc.name, w.Code)
			}
			if len(orch.writes) != 0 {
				t.Errorf("%s: write ran despite a 403", tc.name)
			}
		})
	}
}

func TestSecretsGetNamesOnly(t *testing.T) {
	orch := scaffoldedOrch()
	orch.listResult = docker.SecretNames{
		Dotenv: []string{"BRAVE_KEY"},
		JSON:   []string{"OPENAI_KEY"},
		Native: []string{"web.brave"},
		File:   []string{"token.pem"},
	}
	s := testServer(orch, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsReq(t, http.MethodGet, "tenant_id="+tenantT+"&subs_acc_id="+subsX, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, name := range []string{"BRAVE_KEY", "OPENAI_KEY", "web.brave", "token.pem"} {
		if !strings.Contains(body, name) {
			t.Errorf("missing name %q in listing: %s", name, body)
		}
	}
	for _, group := range []string{`"dotenv"`, `"json"`, `"native"`, `"file"`} {
		if !strings.Contains(body, group) {
			t.Errorf("missing format group %q: %s", group, body)
		}
	}
	// The response shape (SecretNames) carries names only; a value can never be
	// present. This sentinel is a value never placed into any name field.
	if strings.Contains(body, "SECRET-VALUE") {
		t.Errorf("listing leaked a value: %s", body)
	}
}

func TestSecretsGetForbidden(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	// read-only grant -> 403 (same chain as chat).
	h := headersFor(t, licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsReq(t, http.MethodGet, "tenant_id="+tenantT+"&subs_acc_id="+subsX, h))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestSecretsDelete(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	q := "tenant_id=" + tenantT + "&subs_acc_id=" + subsX + "&format=dotenv&name=BRAVE_KEY"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsReq(t, http.MethodDelete, q, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.deletes) != 1 || orch.deletes[0].name != "BRAVE_KEY" || orch.deletes[0].format != "dotenv" {
		t.Errorf("delete not recorded: %+v", orch.deletes)
	}
	if len(orch.restarts) != 0 {
		t.Errorf("container bounced %d times after delete, want 0 (DEC-3)", len(orch.restarts))
	}
	if len(orch.workspaceNotices) != 1 {
		t.Errorf("workspace notices after delete = %d, want 1", len(orch.workspaceNotices))
	}
}

func TestSecretsDeleteUnknownFormat400(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	q := "tenant_id=" + tenantT + "&subs_acc_id=" + subsX + "&format=yaml&name=X"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsReq(t, http.MethodDelete, q, goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if len(orch.deletes) != 0 {
		t.Error("delete ran for an unknown format")
	}
}

func TestHealthzUnauthenticated(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// --- projects (agent-projects) ---
//
// Backed by a plain slice: these tests exercise the HTTP layer's authorization,
// status codes and parameter handling, not the store, which has its own tests in
// internal/projects.

func (f *fakeOrch) ListProjects(docker.WorkspaceKey) ([]projects.Project, error) {
	return f.projects, f.projectErr
}

func (f *fakeOrch) CreateProject(_ docker.WorkspaceKey, name, instructions string) (projects.Project, error) {
	if f.projectErr != nil {
		return projects.Project{}, f.projectErr
	}
	p := projects.Project{ID: "p" + name, Name: name, Instructions: instructions}
	f.projects = append(f.projects, p)
	return p, nil
}

func (f *fakeOrch) RenameProject(_ docker.WorkspaceKey, id, name string) (projects.Project, error) {
	return f.mutateProject(id, func(p *projects.Project) { p.Name = name })
}

func (f *fakeOrch) SetProjectInstructions(_ docker.WorkspaceKey, id, instructions string) (projects.Project, error) {
	return f.mutateProject(id, func(p *projects.Project) { p.Instructions = instructions })
}

func (f *fakeOrch) mutateProject(id string, apply func(*projects.Project)) (projects.Project, error) {
	if f.projectErr != nil {
		return projects.Project{}, f.projectErr
	}
	for i := range f.projects {
		if f.projects[i].ID == id {
			apply(&f.projects[i])
			return f.projects[i], nil
		}
	}
	return projects.Project{}, projects.ErrNotFound
}

func (f *fakeOrch) DeleteProject(_ docker.WorkspaceKey, id string) error {
	if f.projectErr != nil {
		return f.projectErr
	}
	for i := range f.projects {
		if f.projects[i].ID == id {
			f.projects = append(f.projects[:i], f.projects[i+1:]...)
			return nil
		}
	}
	return projects.ErrNotFound
}

func (f *fakeOrch) HasProject(_ docker.WorkspaceKey, id string) (bool, error) {
	if id == "" {
		return true, nil
	}
	if f.projectErr != nil {
		return false, f.projectErr
	}
	for _, p := range f.projects {
		if p.ID == id {
			return true, nil
		}
	}
	return false, nil
}
