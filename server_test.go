// Copyright 2020 Kentaro Hibino. All rights reserved.
// Use of this source code is governed by a MIT license
// that can be found in the LICENSE file.

package asynq

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/hibiken/asynq/internal/rdb"
	"github.com/hibiken/asynq/internal/testbroker"
	"github.com/hibiken/asynq/internal/testutil"
	"go.uber.org/goleak"
)

func TestServer(t *testing.T) {
	// https://github.com/go-redis/redis/issues/1029
	ignoreOpt := goleak.IgnoreTopFunction("github.com/redis/go-redis/v9/internal/pool.(*ConnPool).reaper")
	defer goleak.VerifyNone(t, ignoreOpt)

	redisConnOpt := getRedisConnOpt(t)
	c := NewClient(redisConnOpt)
	defer c.Close()
	srv := NewServer(redisConnOpt, Config{
		Concurrency: 10,
		LogLevel:    testLogLevel,
	})

	// no-op handler
	h := func(ctx context.Context, task *Task) error {
		return nil
	}

	err := srv.Start(HandlerFunc(h))
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Enqueue(NewTask("send_email", testutil.JSON(map[string]interface{}{"recipient_id": 123})))
	if err != nil {
		t.Errorf("could not enqueue a task: %v", err)
	}

	_, err = c.Enqueue(NewTask("send_email", testutil.JSON(map[string]interface{}{"recipient_id": 456})), ProcessIn(1*time.Hour))
	if err != nil {
		t.Errorf("could not enqueue a task: %v", err)
	}

	srv.Shutdown()
}

func TestServerRun(t *testing.T) {
	// https://github.com/go-redis/redis/issues/1029
	ignoreOpt := goleak.IgnoreTopFunction("github.com/redis/go-redis/v9/internal/pool.(*ConnPool).reaper")
	defer goleak.VerifyNone(t, ignoreOpt)

	signalWindows := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)

		mux := NewServeMux()
		srv := NewServer(RedisClientOpt{Addr: ":6379"}, Config{LogLevel: testLogLevel})
		if err := srv.Start(mux); err != nil {
			t.Error("start server:", err)
		}
		if runtime.GOOS == "windows" {
			<-signalWindows
		} else {
			srv.waitForSignals()
		}
		srv.Shutdown()
	}()
	time.Sleep(1 * time.Second)

	// Make sure server exits when receiving TERM signal.
	go func() {
		if runtime.GOOS == "windows" {
			// It is not implemented to signal Interrupt on Windows
			signalWindows <- struct{}{}
			return
		}

		p, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Error("find process:", err)
			return
		}
		err = p.Signal(syscall.SIGTERM)
		if err != nil {
			t.Error("signal:", err)
			return
		}
	}()

	select {
	case <-time.After(10 * time.Second):
		t.Error("stop server: server did not stop after receiving TERM signal")
	case <-done:
	}
}

func TestServerErrServerClosed(t *testing.T) {
	srv := NewServer(RedisClientOpt{Addr: ":6379"}, Config{LogLevel: testLogLevel})
	handler := NewServeMux()
	if err := srv.Start(handler); err != nil {
		t.Fatal(err)
	}
	srv.Shutdown()
	err := srv.Start(handler)
	if err != ErrServerClosed {
		t.Errorf("Restarting server: (*Server).Start(handler) = %v, want ErrServerClosed error", err)
	}
}

func TestServerErrNilHandler(t *testing.T) {
	srv := NewServer(RedisClientOpt{Addr: ":6379"}, Config{LogLevel: testLogLevel})
	err := srv.Start(nil)
	if err == nil {
		t.Error("Starting server with nil handler: (*Server).Start(nil) did not return error")
		srv.Shutdown()
	}
}

func TestServerErrServerRunning(t *testing.T) {
	srv := NewServer(RedisClientOpt{Addr: ":6379"}, Config{LogLevel: testLogLevel})
	handler := NewServeMux()
	if err := srv.Start(handler); err != nil {
		t.Fatal(err)
	}
	err := srv.Start(handler)
	if err == nil {
		t.Error("Calling (*Server).Start(handler) on already running server did not return error")
	}
	srv.Shutdown()
}

func TestServerWithRedisDown(t *testing.T) {
	// Make sure that server does not panic and exit if redis is down.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panic occurred: %v", r)
		}
	}()
	r := rdb.NewRDB(setup(t))
	testBroker := testbroker.NewTestBroker(r)
	srv := NewServer(RedisClientOpt{Addr: ":6379"}, Config{LogLevel: testLogLevel})
	srv.broker = testBroker
	srv.forwarder.broker = testBroker
	srv.heartbeater.broker = testBroker
	srv.processor.broker = testBroker
	srv.subscriber.broker = testBroker
	testBroker.Sleep()

	// no-op handler
	h := func(ctx context.Context, task *Task) error {
		return nil
	}

	err := srv.Start(HandlerFunc(h))
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(3 * time.Second)

	srv.Shutdown()
}

func TestServerWithFlakyBroker(t *testing.T) {
	// Make sure that server does not panic and exit if redis is down.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panic occurred: %v", r)
		}
	}()
	r := rdb.NewRDB(setup(t))
	testBroker := testbroker.NewTestBroker(r)
	redisConnOpt := getRedisConnOpt(t)
	srv := NewServer(redisConnOpt, Config{LogLevel: testLogLevel})
	srv.broker = testBroker
	srv.forwarder.broker = testBroker
	srv.heartbeater.broker = testBroker
	srv.processor.broker = testBroker
	srv.subscriber.broker = testBroker

	c := NewClient(redisConnOpt)

	h := func(ctx context.Context, task *Task) error {
		// force task retry.
		if task.Type() == "bad_task" {
			return fmt.Errorf("could not process %q", task.Type())
		}
		time.Sleep(2 * time.Second)
		return nil
	}

	err := srv.Start(HandlerFunc(h))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		_, err := c.Enqueue(NewTask("enqueued", nil), MaxRetry(i))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Enqueue(NewTask("bad_task", nil))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Enqueue(NewTask("scheduled", nil), ProcessIn(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}

	// simulate redis going down.
	testBroker.Sleep()

	time.Sleep(3 * time.Second)

	// simulate redis comes back online.
	testBroker.Wakeup()

	time.Sleep(3 * time.Second)

	srv.Shutdown()
}

func TestLogLevel(t *testing.T) {
	tests := []struct {
		flagVal string
		want    LogLevel
		wantStr string
	}{
		{"debug", DebugLevel, "debug"},
		{"Info", InfoLevel, "info"},
		{"WARN", WarnLevel, "warn"},
		{"warning", WarnLevel, "warn"},
		{"Error", ErrorLevel, "error"},
		{"fatal", FatalLevel, "fatal"},
	}

	for _, tc := range tests {
		level := new(LogLevel)
		if err := level.Set(tc.flagVal); err != nil {
			t.Fatal(err)
		}
		if *level != tc.want {
			t.Errorf("Set(%q): got %v, want %v", tc.flagVal, level, &tc.want)
			continue
		}
		if got := level.String(); got != tc.wantStr {
			t.Errorf("String() returned %q, want %q", got, tc.wantStr)
		}
	}
}
