package main

import (
	"testing"
	"time"
)

func TestRunWindow(t *testing.T) {
	prep := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		entry      time.Duration // 준비 상품 오픈부터 진입 상품 오픈까지
		start, end time.Duration // 준비 상품 오픈 기준
	}{
		{"기본(30분 뒤)", 30 * time.Minute, 0, 15 * time.Minute},
		{"75분 뒤", 75 * time.Minute, 15 * time.Minute, 60 * time.Minute},
		// 준비 상품 오픈에 실행하면 open_at 이 1시간 넘게 남는다. 시작을 open_at - 1시간으로 늦춘다
		{"2시간 뒤", 2 * time.Hour, time.Hour, 105 * time.Minute},
	} {
		start, end := runWindow(prep, prep.Add(tc.entry), time.Hour)
		if !start.Equal(prep.Add(tc.start)) || !end.Equal(prep.Add(tc.end)) {
			t.Errorf("%s: %s ~ %s", tc.name, start.Sub(prep), end.Sub(prep))
		}
		if !start.Before(end) {
			t.Errorf("%s: 실행 구간이 비었다", tc.name)
		}
		if prep.Add(tc.entry).Sub(start) > time.Hour {
			t.Errorf("%s: 시작에 실행하면 open_at 이 1시간 밖이다", tc.name)
		}
	}
}
