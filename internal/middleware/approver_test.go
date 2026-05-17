package middleware

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestApprover_RequiresApproval_DefaultPolicy(t *testing.T) {
	p := NewPolicyApprover([]string{ToolExecuteScript, ToolWritePatch}, false, time.Second)
	if req, _ := p.RequiresApproval(ToolExecuteScript, nil); !req {
		t.Error("execute_script should require approval")
	}
	if req, _ := p.RequiresApproval(ToolWritePatch, nil); !req {
		t.Error("write_patch should require approval")
	}
	if req, _ := p.RequiresApproval(ToolReadFile, nil); req {
		t.Error("read_file should NOT require approval")
	}
	if req, _ := p.RequiresApproval(ToolListDir, nil); req {
		t.Error("list_dir should NOT require approval")
	}
}

func TestApprover_AutoGrantBypasses(t *testing.T) {
	p := NewPolicyApprover([]string{ToolExecuteScript}, true, time.Second)
	if req, _ := p.RequiresApproval(ToolExecuteScript, nil); req {
		t.Error("auto-grant should bypass approval")
	}
}

func TestApprover_Grant(t *testing.T) {
	p := NewPolicyApprover([]string{ToolExecuteScript}, false, time.Second)
	p.Register("req-1")
	defer p.Cancel("req-1")
	go func() {
		time.Sleep(20 * time.Millisecond)
		p.Signal("req-1", true)
	}()
	granted, err := p.Await(context.Background(), "req-1")
	if err != nil || !granted {
		t.Fatalf("granted=%v err=%v", granted, err)
	}
}

func TestApprover_Deny(t *testing.T) {
	p := NewPolicyApprover([]string{ToolExecuteScript}, false, time.Second)
	p.Register("req-2")
	defer p.Cancel("req-2")
	go p.Signal("req-2", false)
	granted, err := p.Await(context.Background(), "req-2")
	if !errors.Is(err, ErrApprovalDenied) || granted {
		t.Fatalf("want ErrApprovalDenied, got granted=%v err=%v", granted, err)
	}
}

func TestApprover_Timeout(t *testing.T) {
	p := NewPolicyApprover([]string{ToolExecuteScript}, false, 50*time.Millisecond)
	p.Register("req-3")
	defer p.Cancel("req-3")
	granted, err := p.Await(context.Background(), "req-3")
	if !errors.Is(err, ErrApprovalTimeout) || granted {
		t.Fatalf("want ErrApprovalTimeout, got granted=%v err=%v", granted, err)
	}
}

func TestApprover_CtxCancel(t *testing.T) {
	p := NewPolicyApprover([]string{ToolExecuteScript}, false, time.Second)
	p.Register("req-4")
	defer p.Cancel("req-4")
	ctx, cancel := context.WithCancel(context.Background())
	var (
		gotGranted bool
		gotErr     error
		done       atomic.Bool
	)
	go func() {
		gotGranted, gotErr = p.Await(ctx, "req-4")
		done.Store(true)
	}()
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if done.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !done.Load() {
		t.Fatal("Await did not return after ctx cancel")
	}
	if gotGranted || !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("got granted=%v err=%v", gotGranted, gotErr)
	}
}

func TestApprover_UnknownID(t *testing.T) {
	p := NewPolicyApprover(nil, false, time.Second)
	_, err := p.Await(context.Background(), "ghost")
	if !errors.Is(err, ErrApprovalUnknownID) {
		t.Fatalf("want ErrApprovalUnknownID, got %v", err)
	}
}

func TestApprover_LateSignalDropped(t *testing.T) {
	p := NewPolicyApprover([]string{ToolExecuteScript}, false, 50*time.Millisecond)
	p.Register("req-late")
	_, _ = p.Await(context.Background(), "req-late") // times out
	p.Cancel("req-late")
	// Signal arriving after cancel must not panic and must not leak state.
	p.Signal("req-late", true)
}
