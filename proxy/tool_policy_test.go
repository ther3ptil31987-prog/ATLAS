package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The safety rules follow what a tool does, not one tool's name. Keyed on
// names, run_background ran any command without the deny-list or a prompt,
// search_files read credential files and followed symlinks out of the
// workspace, and a credential file could be moved to a readable name.

func commandArgs(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func TestOneCommandPolicyForEveryCommandTool(t *testing.T) {
	denied := []string{
		"rm -rf /", "env rm -rf /", "nohup rm -rf /", "nice -n 10 rm -rf /",
		"(rm -rf /)", "`rm -rf /`", "echo $(rm -rf /)", "timeout 5s rm -rf .",
		"exec rm -rf *", "command rm -rf /workspace", "builtin rm -rf ./",
		"doas rm -rf ~", "FOO=1 rm -rf /", `bash -c "cd src && rm -rf ."`,
		"time mkfs.ext4 /dev/sda1", "env dd if=/dev/zero of=/dev/sda",
		"find . -name '*.py' -delete", ":(){ :|:& };:",
	}
	allowed := []string{
		"grep mkfs docs.txt", "echo 'rm -rf /' > warn.txt", `echo "rm -rf /"`,
		"nohup python app.py > app.log 2>&1 &", "env FLASK_APP=app.py flask run",
		"timeout 5 python app.py", "rm -rf build __pycache__", "dd if=a.bin of=b.bin",
	}
	for tool := range commandTools {
		for _, cmd := range denied {
			if deny, _ := shouldDenyToolCall(tool, commandArgs(cmd)); !deny {
				t.Errorf("%s %q was allowed", tool, cmd)
			}
		}
		for _, cmd := range allowed {
			if deny, why := shouldDenyToolCall(tool, commandArgs(cmd)); deny {
				t.Errorf("%s %q was refused: %s", tool, cmd, why)
			}
		}
	}
}

// Every tool in the registry is classified by what it does, so a new tool
// cannot fall outside the policy by having a new name.
func TestEveryToolIsUnderThePolicyForWhatItDoes(t *testing.T) {
	ownApproval := map[string]bool{"delete_file": true, "move_file": true, "stop_background": true}
	ask := &AgentContext{PermissionMode: PermissionDefault}
	for _, tool := range allTools() {
		switch {
		case tool.ReadOnly:
		case commandTools[tool.Name]:
			if !needsPermission(ask, tool.Name, json.RawMessage(`{}`)) {
				t.Errorf("%s runs commands without asking in default mode", tool.Name)
			}
			if deny, _ := shouldDenyToolCall(tool.Name, commandArgs("env rm -rf /")); !deny {
				t.Errorf("%s runs a command the policy refuses", tool.Name)
			}
		case pathWriteTools[tool.Name]:
			if deny, _ := shouldDenyToolCall(tool.Name, json.RawMessage(`{"path":"config/.env"}`)); !deny {
				t.Errorf("%s writes .env", tool.Name)
			}
		case ownApproval[tool.Name]:
		default:
			t.Errorf("%s changes something and is under no safety rule: classify it", tool.Name)
		}
	}
	if !needsPermission(ask, "delete_file", json.RawMessage(`{}`)) {
		t.Error("delete_file stopped asking")
	}
}

func TestBackgroundCommandsAskLikeCommands(t *testing.T) {
	for _, mode := range []PermissionMode{PermissionDefault, PermissionAcceptEdits} {
		ctx := &AgentContext{PermissionMode: mode}
		if !needsPermission(ctx, "run_background", commandArgs("python app.py")) {
			t.Errorf("mode %d: run_background did not ask", mode)
		}
	}
	if needsPermission(&AgentContext{PermissionMode: PermissionYolo}, "run_background", commandArgs("python app.py")) {
		t.Error("yolo asked")
	}
}

func TestSearchReturnsNoCredentialOrOutsideFile(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "token.txt")
	files := map[string]string{
		".env":             "API_KEY=sk-live-SECRET\n",
		".ssh/id_ed25519":  "SECRET-KEY-MATERIAL\n",
		"app.py":           "import os\nKEY = os.environ['API_KEY']\n",
		"config/prod.pem":  "SECRET-PEM\n",
		".env.example":     "API_KEY=changeme\n",
		"notes/readme.txt": "see app.py\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(outside, []byte("TOKEN-OUTSIDE-WORKSPACE\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "linked.txt")); err != nil {
		t.Skip("no symlinks here:", err)
	}

	res := executeToolCall("search_files", json.RawMessage(`{"pattern":"."}`), newPermCtx(dir))
	if res == nil || !res.Success {
		t.Fatalf("search failed: %+v", res)
	}
	var out SearchFilesOutput
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	body := string(res.Data)
	for _, secret := range []string{"sk-live-SECRET", "SECRET-KEY-MATERIAL", "SECRET-PEM", "TOKEN-OUTSIDE-WORKSPACE"} {
		if strings.Contains(body, secret) {
			t.Errorf("search returned %s", secret)
		}
	}
	if !strings.Contains(body, "os.environ") || !strings.Contains(body, "changeme") {
		t.Errorf("search lost ordinary files: %s", body)
	}
	if out.SkippedCredentialFiles != 3 {
		t.Errorf("skipped_credential_files = %d, want 3", out.SkippedCredentialFiles)
	}
}

func TestACredentialFileCannotBeMovedToAReadableName(t *testing.T) {
	for _, src := range []string{".env", "certs/server.pem", ".ssh/id_rsa"} {
		args, _ := json.Marshal(MoveFileInput{Source: src, Destination: "notes.txt"})
		if deny, _ := shouldDenyToolCall("move_file", args); !deny {
			t.Errorf("move_file %s -> notes.txt was allowed", src)
		}
	}
	args, _ := json.Marshal(MoveFileInput{Source: "old.py", Destination: "src/new.py"})
	if deny, why := shouldDenyToolCall("move_file", args); deny {
		t.Errorf("an ordinary move was refused: %s", why)
	}
}

// Approval is the only per-command control, so the prompt shows the whole
// command. Cut at 100 bytes, the tail of a chain was never seen.
func TestApprovalShowsTheWholeCommand(t *testing.T) {
	long := "cd tests && python3 -m pytest -q test_api.py test_models.py test_views.py 2>&1 | tail -n 40 && cd .. && rm -rf src/legacy data/ && git checkout -- ."
	for tool := range commandTools {
		if got := describeToolCall(tool, commandArgs(long)); !strings.HasSuffix(got, "git checkout -- .") {
			t.Errorf("%s prompt lost the tail: %q", tool, got)
		}
	}
	if got := describeToolCall("stop_background", json.RawMessage(`{"job_id":"job7"}`)); !strings.Contains(got, "job7") {
		t.Errorf("stop_background prompt does not name the job: %q", got)
	}
}

// A deletion is approved one file at a time, whatever a client sends.
func TestASessionApprovalNeverCoversADeletion(t *testing.T) {
	ctx := &AgentContext{PermissionMode: PermissionDefault,
		AllowedTools: map[string]bool{"delete_file": true, "write_file": true}}
	if !needsPermission(ctx, "delete_file", json.RawMessage(`{"path":"a.py"}`)) {
		t.Error("a session approval covered a deletion")
	}
	if needsPermission(ctx, "write_file", json.RawMessage(`{"path":"a.py"}`)) {
		t.Error("a session approval for write_file stopped working")
	}
	turn := &AgentContext{PermissionMode: PermissionDefault}
	turn.allowToolForTurn("delete_file")
	if !needsPermission(turn, "delete_file", json.RawMessage(`{"path":"b.py"}`)) {
		t.Error("a session-scoped answer covered later deletions in the turn")
	}
}
