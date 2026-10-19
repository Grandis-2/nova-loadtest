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
