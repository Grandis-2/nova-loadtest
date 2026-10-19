package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseURI(t *testing.T) {
	const id = "lt-20261020t0630-1"
	b, p, err := parseURI("s3://bucket-x/results/"+id+"/", "results", id)
	if err != nil || b != "bucket-x" || p != "results/"+id+"/" {
		t.Fatalf("%q %q %v", b, p, err)
	}
	for _, bad := range []string{
		"",
		"https://bucket-x/results/" + id + "/",
		"s3://bucket-x/results/lt-20261020t0630-2/", // 다른 시험
		"s3://bucket-x/tokens/" + id + "/",          // 다른 종류
		"s3://bucket-x/results/" + id,               // 끝 / 없음
		"s3:///results/" + id + "/",                 // 버킷 없음
	} {
		if _, _, err := parseURI(bad, "results", id); err == nil {
			t.Errorf("%q를 거절해야 한다", bad)
		}
	}
}

func TestWaitUntil(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	if err := waitUntil("", sigs); err != nil {
		t.Fatal("비어 있으면 바로 시작한다")
	}
	if err := waitUntil(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), sigs); err != nil {
		t.Fatal("지난 시각이면 바로 시작한다")
	}
	if err := waitUntil("내일", sigs); err == nil {
		t.Fatal("형식이 틀리면 실패해야 한다")
	}
	start := time.Now()
	if err := waitUntil(time.Now().Add(1500*time.Millisecond).UTC().Format(time.RFC3339), sigs); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("시작 시각까지 기다려야 한다")
	}
}

func TestWaitUntilStopsOnSignal(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGTERM
	err := waitUntil(time.Now().Add(time.Hour).UTC().Format(time.RFC3339), sigs)
	if err == nil || !strings.Contains(err.Error(), "띄우지 않았다") {
		t.Fatalf("기다리는 중에 멈추면 k6를 띄우지 않아야 한다: %v", err)
	}
}

// 토큰을 받는 동안 멈춤 신호가 왔으면, 시작 시각이 비었거나 지났어도 k6를 띄우지 않는다
func TestWaitUntilPendingSignal(t *testing.T) {
	for _, startAt := range []string{"", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)} {
		sigs := make(chan os.Signal, 1)
		sigs <- syscall.SIGTERM
		if err := waitUntil(startAt, sigs); err == nil {
			t.Fatalf("START_AT %q: 이미 받은 신호가 있으면 띄우지 않아야 한다", startAt)
		}
	}
}

// k6 대신 셸 스크립트를 띄워 종료 코드를 본다
func fakeK6(t *testing.T, body string) {
	t.Helper()
	p := t.TempDir() + "/k6"
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("K6_BIN", p)
	t.Setenv("K6_SCRIPT", "/dev/null")
}

func TestRunK6ExitCodes(t *testing.T) {
	fakeK6(t, "exit 99")
	if code, detail := runK6(make(chan os.Signal)); code != 99 || detail != "" {
		t.Fatalf("k6의 종료 코드를 그대로 돌려줘야 한다: %d %q", code, detail)
	}

	// 신호로 죽으면(예: 메모리 부족 SIGKILL) -1 대신 128 + 신호 번호와 신호 이름을 남긴다
	fakeK6(t, "kill -9 $$")
	code, detail := runK6(make(chan os.Signal))
	if code != 128+9 || !strings.Contains(detail, "killed") {
		t.Fatalf("SIGKILL이면 137과 신호 이름이어야 한다: %d %q", code, detail)
	}
}
