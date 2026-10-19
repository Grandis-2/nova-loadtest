package mint

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
)

type fakeSigner struct{}

func (fakeSigner) Sign(sub string, _ time.Time, _ time.Duration) (string, error) {
	return "tok:" + sub, nil
}

func cfg(t *testing.T) *config.Config {
	c, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSplit(t *testing.T) {
	parts := Split(Range{Start: 10, Count: 10}, 3)
	want := []Range{{10, 4}, {14, 3}, {17, 3}}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("%v != %v", parts, want)
		}
	}
	if p := Split(Range{Start: 1, Count: 0}, 2); p[0].Count != 0 || p[1].Count != 0 {
		t.Fatalf("빈 범위: %v", p)
	}
}

func TestSub(t *testing.T) {
	if got := Sub(42); got != "10ad7e57-0000-7000-8000-000000000042" {
		t.Fatal(got)
	}
}

func TestFiles(t *testing.T) {
	r := Request{Members: Members{B: Range{1, 5}, C: Range{6, 2}, A: Range{8, 7}}, Tasks: 2, TTL: time.Hour}
	files, err := Files(fakeSigner{}, r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 6 {
		t.Fatalf("풀 셋 × 태스크 둘 = 파일 여섯 개여야 한다: %d", len(files))
	}
	seen := map[int64]bool{}
	for _, f := range files {
		var es []Entry
		if err := json.Unmarshal(f.Body, &es); err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if seen[e.N] {
				t.Fatalf("회원 %d가 두 파일에 있다", e.N)
			}
			seen[e.N] = true
			if e.Sub != Sub(e.N) || e.Token != "tok:"+e.Sub {
				t.Fatalf("원소가 이상하다: %+v", e)
			}
		}
	}
	if len(seen) != 14 {
		t.Fatalf("회원 14명이 모두 있어야 한다: %d", len(seen))
	}
}

func TestEmptyPoolStillWritesFile(t *testing.T) {
	files, err := Files(fakeSigner{}, Request{Members: Members{A: Range{1, 3}}, Tasks: 1, TTL: time.Hour}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name, "b-") && string(f.Body) != "[]" {
			t.Fatalf("빈 풀은 []여야 한다: %s", f.Body)
		}
	}
}

func TestCheck(t *testing.T) {
	c := cfg(t)
	ok := Request{Members: Members{B: Range{1, 1000}, C: Range{1001, 100}, A: Range{1101, 5000}}, Tasks: 3, TTL: 2 * time.Hour}
	if err := Check(c, ok); err != nil {
		t.Fatalf("통과해야 한다: %v", err)
	}
	for _, tc := range []struct {
		name string
		mod  func(*Request)
		want string
	}{
		{"범위 밖(실제 회원)", func(r *Request) { r.Members.A = Range{c.Members.Start + c.Members.Count, 1} }, "범위"},
		{"범위 앞", func(r *Request) { r.Members.B = Range{0, 10} }, "범위"},
		{"유효 시간 초과", func(r *Request) { r.TTL = 7 * time.Hour }, "유효 시간"},
		{"풀 겹침", func(r *Request) { r.Members.C = Range{500, 100} }, "겹친다"},
		{"태스크 0", func(r *Request) { r.Tasks = 0 }, "태스크"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ok
			tc.mod(&r)
			if err := Check(c, r); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("거절해야 한다(%q): %v", tc.want, err)
			}
		})
	}
}
