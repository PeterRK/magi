package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/engine"
	"magi/internal/core/protocol"
	"magi/internal/model"
	"magi/internal/storage"
)

func TestCLIOnlyCurrentDirectoryAndMinimalEntrypoints(t *testing.T) {
	isolateBindings(t)
	ctx, root := context.Background(), t.TempDir()
	t.Chdir(root)
	var out bytes.Buffer
	runner := New(&out, &out)

	for _, args := range [][]string{{"status"}, {"input"}, {"cancel"}, {"run"}, {"--project", root}, {"--bash-path", "/bin/bash"}, {"--git-path", "/usr/bin/git"}, {"--task", "other"}, {"--text", "answer"}, {"--input-file", "answer"}, {"init", "--project", root}} {
		if err := runner.Run(ctx, args); err == nil {
			t.Fatalf("removed entry accepted: %v", args)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".magi")); !os.IsNotExist(err) {
		t.Fatal("rejected command created state")
	}
	noError(t, runner.Run(ctx, []string{"init"}))
	child := filepath.Join(root, "child")
	noError(t, os.Mkdir(child, 0700))
	t.Chdir(child)
	if err := runner.Run(ctx, nil); err == nil {
		t.Fatal("searched parent project")
	}
	if _, err := os.Stat(filepath.Join(child, ".magi")); !os.IsNotExist(err) {
		t.Fatal("ordinary entry initialized child")
	}
}

func TestCLIInputCancellationAndInvalidUTF8PreserveState(t *testing.T) {
	isolateBindings(t)
	ctx, root := context.Background(), t.TempDir()
	t.Chdir(root)
	var out bytes.Buffer
	runner := New(&out, &out)

	noError(t, runner.Run(ctx, []string{"init"}))
	runner.In = bytes.NewReader([]byte{0xff})
	if err := runner.Run(ctx, nil); !protocol.IsCode(err, protocol.CodeInvalidCommand) {
		t.Fatal(err)
	}
	store := openTestProject(t, ctx, root)
	_, found, err := store.Disk().Latest(ctx)
	noError(t, err)
	if found {
		t.Fatal("invalid input created a workflow")
	}
	noError(t, store.Close())
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	runner.In = reader
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := runner.Run(cancelled, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCLIExecutableDiscoveryBindsPaths(t *testing.T) {
	isolateBindings(t)
	t.Chdir(t.TempDir())
	var output bytes.Buffer
	runner := New(&output, &output)
	noError(t, runner.Run(context.Background(), []string{"init"}))
	program := filepath.Join(t.TempDir(), "bash")
	noError(t, os.WriteFile(program, []byte("#!/bin/sh\nprintf configured-bash"), 0700))
	t.Setenv("PATH", filepath.Dir(program)+string(os.PathListSeparator)+os.Getenv("PATH"))
	done := errors.New("configuration checked")
	runner.In = strings.NewReader("")
	runner.modelFactory = func() (protocol.Model, error) {
		t.Setenv("PATH", t.TempDir())
		result, err := protocol.Tools.Execute(context.Background(), protocol.ToolRequest{
			ID: "configured", Name: "bash", Arguments: json.RawMessage(`{"command":"ignored"}`),
		})
		noError(t, err)
		var body struct {
			Stdout string `json:"stdout_tail"`
		}
		noError(t, json.Unmarshal(result.Body, &body))
		if body.Stdout != "configured-bash" {
			t.Fatal("CLI did not retain the discovered executable after PATH changed", body)
		}
		return nil, done
	}
	if err := runner.Run(context.Background(), nil); !errors.Is(err, done) {
		t.Fatal(err)
	}
}

func TestCLICompetingEntryDoesNotConsumeInput(t *testing.T) {
	isolateBindings(t)
	ctx, root := context.Background(), t.TempDir()
	t.Chdir(root)
	var out bytes.Buffer
	runner := New(&out, &out)

	noError(t, runner.Run(ctx, []string{"init"}))
	unlock, err := storage.Lock(root)
	noError(t, err)
	reader := strings.NewReader("must remain unread")
	runner.In = reader
	if err := runner.Run(ctx, nil); !protocol.IsCode(err, protocol.CodeLifecycleConflict) {
		t.Fatal(err)
	}
	if reader.Len() != len("must remain unread") {
		t.Fatal("competing entry consumed input")
	}
	unlock()
	if err := runner.Run(ctx, nil); !errors.Is(err, model.ErrMissingKey) {
		t.Fatal(err)
	}
}

func TestInterruptedColdStartAsksAgainOnNextEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateBindings(t)
		ctx := context.Background()
		t.Chdir(t.TempDir())
		var out, errout bytes.Buffer
		r := New(&out, &errout)

		noError(t, r.Run(ctx, []string{"init"}))
		var entries []*entryObserver
		r.modelFactory = func() (protocol.Model, error) {
			observed := &entryObserver{Model: workflowModel(t)}
			entries = append(entries, observed)
			return observed, nil
		}
		out.Reset()
		r.In = strings.NewReader("")
		firstCtx, cancel := context.WithTimeout(ctx, time.Second)
		if err := r.Run(firstCtx, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		cancel()
		out.Reset()
		r.In = strings.NewReader("Please ship a reviewed delivery.")
		noError(t, r.Run(ctx, nil))
		var second engine.Workflow
		noError(t, json.Unmarshal(out.Bytes(), &second))
		for _, entry := range entries {
			assertColdEntry(t, entry)
		}
		if second.Phase != engine.PhaseDormant {
			t.Fatal("new entry did not route answer to reissued cold-start question")
		}
	})
}

func assertColdEntry(t *testing.T, observed *entryObserver) {
	t.Helper()
	count := 0
	for _, message := range observed.first {
		mode := messageField(message.Content, "workflow_entry")
		if mode == "" {
			continue
		}
		count++
		if mode != "COLD_START" {
			t.Fatalf("unfinished first intake was presented as old-task recovery: %s", message.Content)
		}
	}
	if count != 1 {
		t.Fatalf("cold entry has %d startup records", count)
	}
}

func TestInspectEntryDoesNotStartEngineOrConsumeInput(t *testing.T) {
	isolateBindings(t)
	ctx, root := context.Background(), t.TempDir()
	t.Chdir(root)
	var out bytes.Buffer
	r := New(&out, &out)
	r.In = strings.NewReader("must remain unread")
	r.modelFactory = func() (protocol.Model, error) { t.Fatal("inspect started model"); return nil, nil }
	if err := r.Run(ctx, []string{"inspect"}); err == nil {
		t.Fatal("inspected missing project")
	}
	if _, err := os.Stat(filepath.Join(root, ".magi")); !os.IsNotExist(err) {
		t.Fatal("inspect initialized project")
	}
	p, err := storage.InitProject(ctx, root)
	noError(t, err)
	noError(t, p.Disk().ObserveUsage("WORKER", protocol.TokenUsage{Hit: 30, Miss: 10, Output: 5}))
	noError(t, p.Close())
	for _, args := range [][]string{{"inspect"}, {"inspect", "--usage"}} {
		out.Reset()
		noError(t, r.Run(ctx, args))
		var usage storage.UsageReport
		if len(args) == 1 {
			var report storage.Inspection
			noError(t, json.Unmarshal(out.Bytes(), &report))
			if len(report.Rounds) != 0 || report.FormatVersion != protocol.StorageFormatVersion {
				t.Fatal(report)
			}
			usage = report.Usage
		} else {
			noError(t, json.Unmarshal(out.Bytes(), &usage))
		}
		if usage.Total.TotalTokens.Uint64() != 45 || *usage.Total.CacheHitRate != 0.75 {
			t.Fatal(usage)
		}
	}
	if r.In.(*strings.Reader).Len() != len("must remain unread") {
		t.Fatal("inspect consumed stdin")
	}
	if err := r.Run(ctx, []string{"inspect", "extra"}); err == nil {
		t.Fatal("accepted unsupported inspect arguments")
	}
}
