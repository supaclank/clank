package hostmux

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// A question reply must forward its structured answers (and the reject flag)
// through the mux and Service to the backend's RespondQuestion.
func TestQuestionReply_ForwardsAnswers(t *testing.T) {
	backend := &captureBackend{}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendClaudeCode: &captureBackendManager{backend: backend},
		},
	})
	t.Cleanup(svc.Shutdown)
	m := New(svc, nil)

	dir := initGitRepoMux(t)
	body, err := json.Marshal(agent.StartRequest{
		Backend: agent.BackendClaudeCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s, want 201", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("decode created session id: err=%v body=%s", err, w.Body.String())
	}

	wantAnswers := []agent.QuestionAnswer{
		{Selected: []string{"JWT"}},
		{Custom: "something else"},
	}
	replyBody, _ := json.Marshal(QuestionReplyRequest{Answers: wantAnswers})
	rw := httptest.NewRecorder()
	m.Handler().ServeHTTP(rw, httptest.NewRequest(http.MethodPost,
		"/sessions/"+created.ID+"/questions/perm-7/reply", bytes.NewReader(replyBody)))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("reply status=%d body=%s, want 204", rw.Code, rw.Body.String())
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if !backend.gotQuestionCalled {
		t.Fatal("backend.RespondQuestion was not called")
	}
	if backend.gotQuestionID != "perm-7" {
		t.Errorf("backend received requestID=%q, want perm-7", backend.gotQuestionID)
	}
	if backend.gotQuestionReject {
		t.Error("backend received reject=true, want false")
	}
	if !reflect.DeepEqual(backend.gotQuestionAnswers, wantAnswers) {
		t.Errorf("backend received answers=%+v, want %+v", backend.gotQuestionAnswers, wantAnswers)
	}
}
