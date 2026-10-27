package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

func TestAttachmentContextCannotRunBeforeReview(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX context command fixture")
	}
	for _, name := range []string{"request", "run"} {
		t.Run(name, func(t *testing.T) {
			resetRequestFlags()
			resetRunFlags()
			t.Cleanup(resetRequestFlags)
			t.Cleanup(resetRunFlags)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SHELL", "/bin/sh")
			h := testutil.NewHarness(t)
			t.Chdir(h.ProjectDir)
			h.WriteFile(".slb/config.toml", []byte("[general]\napproval_policy = 'same_host'\nenable_dry_run = false\n[integrations]\nagent_mail_enabled = false\n"), 0600)
			session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
			marker := filepath.Join(h.ProjectDir, "unreviewed-context")
			root := newTestRequestCmd(h.DBPath)
			args := []string{name, "touch reviewed-command", "--session-id", session.ID, "-C", h.ProjectDir,
				"--attach-context", "touch " + hookShellQuote(marker), "--reason", "context must not bypass review", "--json"}
			if name == "run" {
				root = newTestRunCmd(h.DBPath)
				args = append(args, "--yield")
			}
			output, err := executeCommandCapture(t, root, args...)
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("unreviewed attachment command executed: %v\n%s", statErr, output)
			}
			if err == nil || !strings.Contains(err.Error(), "SAFE") {
				t.Fatalf("context requiring review was not refused: %v\n%s", err, output)
			}
			var count int
			if err := h.DB.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("refused context left a request: %d %v", count, err)
			}
		})
	}
}

func TestCollectAttachments_EmptyFlags(t *testing.T) {
	_ = testutil.NewHarness(t) // Ensure test cleanup

	flags := AttachmentFlags{}
	attachments, err := CollectAttachments(context.Background(), flags, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attachments) != 0 {
		t.Errorf("expected 0 attachments with empty flags, got %d", len(attachments))
	}
}

func TestCollectAttachments_FileAttachment(t *testing.T) {
	h := testutil.NewHarness(t)

	// Create a test file
	testFile := filepath.Join(h.ProjectDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	flags := AttachmentFlags{
		Files: []string{testFile},
	}

	attachments, err := CollectAttachments(context.Background(), flags, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(attachments))
	}
	// Attachment has Type, Content, and Metadata fields
	if attachments[0].Type != "file" {
		t.Errorf("expected type 'file', got %q", attachments[0].Type)
	}
	if attachments[0].Content == "" {
		t.Error("expected non-empty content")
	}
}

func TestCollectAttachments_FileNotFound(t *testing.T) {
	_ = testutil.NewHarness(t)

	flags := AttachmentFlags{
		Files: []string{"/nonexistent/path/file.txt"},
	}

	_, err := CollectAttachments(context.Background(), flags, nil)

	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestCollectAttachments_MultipleFiles(t *testing.T) {
	h := testutil.NewHarness(t)

	// Create test files
	file1 := filepath.Join(h.ProjectDir, "file1.txt")
	file2 := filepath.Join(h.ProjectDir, "file2.txt")
	if err := os.WriteFile(file1, []byte("content 1"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	if err := os.WriteFile(file2, []byte("content 2"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	flags := AttachmentFlags{
		Files: []string{file1, file2},
	}

	attachments, err := CollectAttachments(context.Background(), flags, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attachments) != 2 {
		t.Errorf("expected 2 attachments, got %d", len(attachments))
	}
}

func TestCollectAttachments_ContextCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	scope := attachmentTestScope(t, h)

	flags := AttachmentFlags{
		Contexts: []string{"echo hello"},
	}

	attachments, err := CollectAttachments(context.Background(), flags, scope)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(attachments))
	}
	// Context commands produce a "context" type attachment
	if attachments[0].Type != "context" {
		t.Errorf("expected type 'context', got %q", attachments[0].Type)
	}
	if attachments[0].Metadata["policy_check"] != "safe" || attachments[0].Metadata["cwd"] != h.ProjectDir ||
		attachments[0].Metadata["command_hash"] != db.ComputeCommandHash(db.CommandSpec{Raw: "echo hello", Cwd: h.ProjectDir, Shell: true}) {
		t.Fatalf("context lost its actual command/directory/policy provenance: %+v", attachments[0])
	}
}

func TestCollectAttachments_UnknownContextCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	scope := attachmentTestScope(t, h)

	flags := AttachmentFlags{
		Contexts: []string{"nonexistent-command-xyz"},
	}

	_, err := CollectAttachments(context.Background(), flags, scope)
	if err == nil || !strings.Contains(err.Error(), "SAFE") {
		t.Fatalf("unknown command did not require review: %v", err)
	}
}

func TestCollectAttachments_ScreenshotNotFound(t *testing.T) {
	_ = testutil.NewHarness(t)

	flags := AttachmentFlags{
		Screenshots: []string{"/nonexistent/screenshot.png"},
	}

	_, err := CollectAttachments(context.Background(), flags, nil)

	if err == nil {
		t.Fatal("expected error for nonexistent screenshot")
	}
}

func TestAttachmentFlags_Struct(t *testing.T) {
	// Verify AttachmentFlags struct can be used properly
	flags := AttachmentFlags{
		Files:       []string{"file1.txt", "file2.txt"},
		Contexts:    []string{"ls -la", "git status"},
		Screenshots: []string{"screen.png"},
	}

	if len(flags.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(flags.Files))
	}
	if len(flags.Contexts) != 2 {
		t.Errorf("expected 2 contexts, got %d", len(flags.Contexts))
	}
	if len(flags.Screenshots) != 1 {
		t.Errorf("expected 1 screenshot, got %d", len(flags.Screenshots))
	}
}

// TestCollectAttachments_ValidScreenshot tests loading a valid screenshot.
func TestCollectAttachments_ValidScreenshot(t *testing.T) {
	h := testutil.NewHarness(t)

	// Create a minimal valid PNG file (1x1 pixel)
	// PNG signature + IHDR chunk + IDAT chunk + IEND chunk
	pngData := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
		0x00, 0x00, 0x00, 0x0D, // IHDR length
		0x49, 0x48, 0x44, 0x52, // IHDR
		0x00, 0x00, 0x00, 0x01, // width: 1
		0x00, 0x00, 0x00, 0x01, // height: 1
		0x08, 0x02, // 8-bit RGB
		0x00, 0x00, 0x00, // compression, filter, interlace
		0x90, 0x77, 0x53, 0xDE, // CRC
		0x00, 0x00, 0x00, 0x0C, // IDAT length
		0x49, 0x44, 0x41, 0x54, // IDAT
		0x08, 0xD7, 0x63, 0xF8, 0xFF, 0xFF, 0xFF, 0x00, 0x05, 0xFE, 0x02, 0xFE, // compressed data
		0xA2, 0x76, 0xD0, 0x3A, // CRC
		0x00, 0x00, 0x00, 0x00, // IEND length
		0x49, 0x45, 0x4E, 0x44, // IEND
		0xAE, 0x42, 0x60, 0x82, // CRC
	}

	screenshotPath := filepath.Join(h.ProjectDir, "test_screenshot.png")
	if err := os.WriteFile(screenshotPath, pngData, 0644); err != nil {
		t.Fatalf("failed to create test screenshot: %v", err)
	}

	flags := AttachmentFlags{
		Screenshots: []string{screenshotPath},
	}

	attachments, err := CollectAttachments(context.Background(), flags, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(attachments))
	}
	// Screenshot should produce a "screenshot" type attachment
	if attachments[0].Type != "screenshot" {
		t.Errorf("expected type 'screenshot', got %q", attachments[0].Type)
	}
}

// TestCollectAttachments_MixedTypes tests loading multiple attachment types.
func TestCollectAttachments_MixedTypes(t *testing.T) {
	h := testutil.NewHarness(t)
	scope := attachmentTestScope(t, h)

	// Create a test file
	filePath := filepath.Join(h.ProjectDir, "test.txt")
	if err := os.WriteFile(filePath, []byte("test content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	flags := AttachmentFlags{
		Files:    []string{filePath},
		Contexts: []string{"echo hello"},
	}

	attachments, err := CollectAttachments(context.Background(), flags, scope)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(attachments))
	}

	// Verify we got both types
	hasFile := false
	hasContext := false
	for _, a := range attachments {
		if a.Type == "file" {
			hasFile = true
		}
		if a.Type == "context" {
			hasContext = true
		}
	}
	if !hasFile {
		t.Error("expected file attachment")
	}
	if !hasContext {
		t.Error("expected context attachment")
	}
}

func attachmentTestScope(t *testing.T, h *testutil.Harness) *AttachmentCommandScope {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	h.WriteFile(".slb/config.toml", []byte("[general]\nenable_dry_run = false\n[patterns.safe]\npatterns = ['^echo(?:\\s|$)', '^pwd$', '^context-probe$']\n[integrations]\nagent_mail_enabled = false\n"), 0600)
	session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("ContextRequester"))
	return &AttachmentCommandScope{Database: h.DB, ProjectPath: h.ProjectDir,
		WorkingDirectory: h.ProjectDir, SessionID: session.ID}
}

func installAttachmentProbe(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX context command fixture")
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "context-started")
	t.Setenv("TEST_CONTEXT_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if script == "" {
		script = "printf 'started\\n' >> \"$TEST_CONTEXT_MARKER\"\npwd\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "context-probe"), []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatal(err)
	}
	return marker
}

func TestAttachmentContextValidatesScopeBeforeStarting(t *testing.T) {
	for _, scenario := range []string{"missing scope", "unknown session", "ended session", "foreign project", "blocked agent", "missing config", "invalid config", "unbound cwd", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			h := testutil.NewHarness(t)
			scope := attachmentTestScope(t, h)
			marker := installAttachmentProbe(t, "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "missing scope":
				scope = nil
			case "unknown session":
				scope.SessionID = "missing-session"
			case "ended session":
				if err := h.DB.EndSession(scope.SessionID); err != nil {
					t.Fatal(err)
				}
			case "foreign project":
				scope.ProjectPath = t.TempDir()
			case "blocked agent":
				path := filepath.Join(h.ProjectDir, ".slb", "config.toml")
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(original, []byte("[agents]\nblocked = ['ContextRequester']\n")...), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing config":
				scope.ConfigPath = filepath.Join(t.TempDir(), "missing.toml")
			case "invalid config":
				scope.ConfigPath = h.WriteFile("broken.toml", []byte("[general\n"), 0600)
			case "unbound cwd":
				scope.WorkingDirectory = ""
			case "cancelled":
				cancel()
			}
			attachments, err := CollectAttachments(ctx, AttachmentFlags{Contexts: []string{"context-probe"}}, scope)
			if err == nil || len(attachments) != 0 {
				t.Fatalf("invalid context scope was accepted: %+v %v", attachments, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("context subprocess started before scope refusal: %v", err)
			}
		})
	}
}

func TestAttachmentContextValidatesEntireBatchBeforeStarting(t *testing.T) {
	h := testutil.NewHarness(t)
	scope := attachmentTestScope(t, h)
	marker := installAttachmentProbe(t, "")
	attachments, err := CollectAttachments(context.Background(), AttachmentFlags{
		Contexts: []string{"context-probe", "git push --force"},
	}, scope)
	if err == nil || !strings.Contains(err.Error(), "SAFE") || len(attachments) != 0 {
		t.Fatalf("unsafe attachment batch was accepted: %+v %v", attachments, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("earlier context ran before later context was refused: %v", err)
	}
}

func TestAttachmentContextValidatesScreenshotsBeforeStarting(t *testing.T) {
	for _, scenario := range []string{"missing screenshot", "invalid image"} {
		t.Run(scenario, func(t *testing.T) {
			h := testutil.NewHarness(t)
			scope := attachmentTestScope(t, h)
			marker := installAttachmentProbe(t, "")
			screenshot := filepath.Join(h.ProjectDir, "missing.png")
			if scenario == "invalid image" {
				screenshot = h.WriteFile("invalid.png", []byte("not an image"), 0600)
			}
			attachments, err := CollectAttachments(context.Background(), AttachmentFlags{
				Contexts: []string{"context-probe"}, Screenshots: []string{screenshot},
			}, scope)
			if err == nil || !strings.Contains(err.Error(), "loading screenshot") || len(attachments) != 0 {
				t.Fatalf("invalid screenshot batch was accepted or failed for another reason: %+v %v", attachments, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("context ran before the screenshot was refused: %v", err)
			}
		})
	}
}

func TestAttachmentContextReloadsAdmissionPolicy(t *testing.T) {
	for _, name := range []string{"request", "run"} {
		for _, change := range []string{"block requester", "change approval policy"} {
			t.Run(name+"/"+change, func(t *testing.T) {
				resetRequestFlags()
				resetRunFlags()
				t.Cleanup(resetRequestFlags)
				t.Cleanup(resetRunFlags)
				h := testutil.NewHarness(t)
				scope := attachmentTestScope(t, h)
				t.Chdir(h.ProjectDir)
				const initial = "[general]\napproval_policy = 'same_host'\nenable_dry_run = false\n[patterns.safe]\npatterns = ['^context-probe$']\n[integrations]\nagent_mail_enabled = false\n"
				scope.ConfigPath = h.WriteFile("selected.toml", []byte(initial), 0600)
				next := strings.Replace(initial, "same_host", "independent", 1)
				if change == "block requester" {
					next = initial + "[agents]\nblocked = ['ContextRequester']\n"
				}
				t.Setenv("TEST_CONTEXT_POLICY", scope.ConfigPath)
				t.Setenv("TEST_CONTEXT_NEXT_POLICY", next)
				marker := installAttachmentProbe(t, "printf 'started\\n' >> \"$TEST_CONTEXT_MARKER\"\nprintf '%s' \"$TEST_CONTEXT_NEXT_POLICY\" > \"$TEST_CONTEXT_POLICY\"\npwd\n")
				mainMarker := filepath.Join(h.ProjectDir, "must-not-execute-main")
				root := newTestRequestCmd(h.DBPath)
				args := []string{name, "touch " + hookShellQuote(mainMarker), "--session-id", scope.SessionID,
					"-C", h.ProjectDir, "--config", scope.ConfigPath, "--attach-context", "context-probe",
					"--reason", "use policy after context collection", "--json"}
				if name == "run" {
					root = newTestRunCmd(h.DBPath)
					args = append(args, "--yield")
				}
				output, runErr := executeCommandCapture(t, root, args...)
				if data, err := os.ReadFile(marker); err != nil || string(data) != "started\n" {
					t.Fatalf("the single SAFE context did not run exactly once: %q %v\n%s", data, err, output)
				}
				if data, err := os.ReadFile(scope.ConfigPath); err != nil || string(data) != next {
					t.Fatalf("context did not change the selected policy: %q %v", data, err)
				}
				if _, err := os.Stat(mainMarker); !os.IsNotExist(err) {
					t.Fatalf("main command ran without review: %v\n%s", err, output)
				}
				var count int
				if err := h.DB.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if change == "block requester" {
					if runErr == nil || !strings.Contains(runErr.Error(), "blocked") || count != 0 {
						t.Fatalf("new requester block was ignored: requests=%d err=%v\n%s", count, runErr, output)
					}
					return
				}
				if runErr != nil || count != 1 {
					t.Fatalf("request was not admitted under the replacement policy: requests=%d err=%v\n%s", count, runErr, output)
				}
				var response struct {
					RequestID      string            `json:"request_id"`
					ApprovalPolicy db.ApprovalPolicy `json:"approval_policy"`
				}
				if err := json.Unmarshal([]byte(output), &response); err != nil || response.RequestID == "" || response.ApprovalPolicy != db.ApprovalPolicyIndependent {
					t.Fatalf("response retained the pre-context policy: %+v %v\n%s", response, err, output)
				}
				request, err := h.DB.GetRequest(response.RequestID)
				if err != nil || request.ApprovalPolicy != db.ApprovalPolicyIndependent || request.Status != db.StatusPending || request.Execution != nil {
					t.Fatalf("persisted request retained stale authorization: %+v %v", request, err)
				}
			})
		}
	}
}

func TestAttachmentContextRechecksSelectedPolicyAndBindsCWD(t *testing.T) {
	h := testutil.NewHarness(t)
	scope := attachmentTestScope(t, h)
	// The selected file allows the probe while project defaults do not. The
	// probe changes that selected file; the next context must reread it.
	scope.ConfigPath = h.WriteFile("selected.toml", []byte("[patterns.safe]\npatterns = ['^context-probe$', '^echo second$']\n"), 0600)
	h.WriteFile(".slb/config.toml", []byte("[patterns.safe]\npatterns = []\n"), 0600)
	t.Setenv("TEST_CONTEXT_POLICY", scope.ConfigPath)
	marker := installAttachmentProbe(t, "pwd > \"$TEST_CONTEXT_MARKER\"\nprintf '[patterns.safe]\\npatterns = []\\n' > \"$TEST_CONTEXT_POLICY\"\n")
	attachments, err := CollectAttachments(context.Background(), AttachmentFlags{
		Contexts: []string{"context-probe", "echo second"},
	}, scope)
	if err == nil || !strings.Contains(err.Error(), "rechecking context command 2") || len(attachments) != 0 {
		t.Fatalf("changed selected policy was ignored: %+v %v", attachments, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(data)) != h.ProjectDir {
		t.Fatalf("allowed context did not use the checked working directory: %q %v", data, err)
	}
}
