package main

import (
	"errors"
	"fmt"
	"net/url"
	"syscall"
	"testing"
	"time"
)

func TestIsTransient(t *testing.T) {
	reset := &url.Error{Op: "Post", URL: "https://portal.fedsfm.ru/CommandManager/Execute",
		Err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)}
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmt.Errorf("лента info: %w", reset), true},
		{fmt.Errorf("лента info: %v", reset), true}, // обёрнуто без %w
		{errors.New("download xml: неожиданный статус 503"), true},
		{errors.New("download xml: неожиданный статус 404"), false},
		{errors.New("лента info: не удалось определить messageId"), false},
	}
	for _, c := range cases {
		if got := isTransient(c.err); got != c.want {
			t.Errorf("isTransient(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestRetryWaitLoop(t *testing.T) {
	retryDelay = func(int) time.Duration { return 0 }
	calls := 0
	call := func() (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New("read: connection reset by peer")
		}
		return "ok", nil
	}
	v, err := call()
	for a := 1; retryWait(err, a, 3, "тест"); a++ {
		v, err = call()
	}
	if err != nil || v != "ok" || calls != 3 {
		t.Fatalf("v=%q err=%v calls=%d", v, err, calls)
	}

	calls = 0
	perm := func() error { calls++; return errors.New("messageId не найден") }
	err = perm()
	for a := 1; retryWait(err, a, 3, "тест"); a++ {
		err = perm()
	}
	if calls != 1 {
		t.Fatalf("постоянная ошибка не должна повторяться, calls=%d", calls)
	}

	if !errors.Is(fmt.Errorf("%w: info", errPartialRun), errPartialRun) {
		t.Fatal("errPartialRun должен распознаваться через errors.Is")
	}
}
