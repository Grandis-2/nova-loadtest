// Package mint는 회원 풀을 k6 태스크 수만큼 나눠 태스크와 풀마다 토큰 파일을 만든다.
// 파일 이름은 {a,b,c}-<태스크 번호>.json이고, 원소는 {n, sub, token}이다.
package mint

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
)

// Range는 회원 번호 범위 [Start, Start+Count)이다(계획의 members와 같은 형식).
type Range struct {
	Start int64 `json:"start"`
	Count int64 `json:"count"`
}

type Members struct {
	A Range `json:"a"`
	B Range `json:"b"`
	C Range `json:"c"`
}

// Entry는 토큰 파일의 원소다. k6는 n으로 접수의 Idempotency-Key를 만든다.
type Entry struct {
	N     int64  `json:"n"`
	Sub   string `json:"sub"`
	Token string `json:"token"`
}

// Signer는 sub의 토큰을 만든다(jwt.Signer).
type Signer interface {
	Sign(sub string, now time.Time, ttl time.Duration) (string, error)
}

// Sub는 회원 번호로 customers.id를 만든다. 규칙은 be와 맞춘다(R-1).
func Sub(n int64) string { return fmt.Sprintf("10ad7e57-0000-7000-8000-%012d", n) }

// Request는 발급 요청이다.
type Request struct {
	Members Members
	Tasks   int
	TTL     time.Duration
}

// Check는 요청이 시험 회원 범위와 유효 시간 상한 안인지 본다. 범위는 실행 파일에 들어 있는 설정에서 온다.
func Check(cfg *config.Config, r Request) error {
	var errs []error
	if r.Tasks < 1 || r.Tasks > cfg.Runner.MaxTasks {
		errs = append(errs, fmt.Errorf("태스크 수 %d는 1에서 %d 사이여야 한다", r.Tasks, cfg.Runner.MaxTasks))
	}
	// MEMBERS가 비었거나(null, {}) 모든 풀이 0이면 잘못 넘어온 요청이다. 빈 파일을 올리고 성공하지 않게 거절한다
	if r.Members.A.Count+r.Members.B.Count+r.Members.C.Count <= 0 {
		errs = append(errs, errors.New("회원이 하나도 없다(MEMBERS를 확인한다)"))
	}
	if r.TTL <= 0 || r.TTL > cfg.Members.MaxTokenTTL.D() {
		errs = append(errs, fmt.Errorf("유효 시간 %s는 0보다 크고 %s 이하여야 한다", r.TTL, cfg.Members.MaxTokenTTL.D()))
	}
	pools := []struct {
		name string
		r    Range
	}{{"a", r.Members.A}, {"b", r.Members.B}, {"c", r.Members.C}}
	for _, p := range pools {
		if p.r.Count < 0 {
			errs = append(errs, fmt.Errorf("풀 %s의 개수가 음수다", p.name))
			continue
		}
		if p.r.Count == 0 {
			continue
		}
		last := p.r.Start + p.r.Count - 1
		if !cfg.Members.Contains(p.r.Start) || !cfg.Members.Contains(last) {
			errs = append(errs, fmt.Errorf("풀 %s의 회원 %d~%d가 시험 회원 범위 %d~%d 밖이다",
				p.name, p.r.Start, last, cfg.Members.Start, cfg.Members.Start+cfg.Members.Count-1))
		}
	}
	// 풀끼리 겹치면 한 회원이 두 흐름에 쓰인다
	for i := range pools {
		for j := i + 1; j < len(pools); j++ {
			a, b := pools[i].r, pools[j].r
			if a.Count > 0 && b.Count > 0 && a.Start < b.Start+b.Count && b.Start < a.Start+a.Count {
				errs = append(errs, fmt.Errorf("풀 %s와 %s의 범위가 겹친다", pools[i].name, pools[j].name))
			}
		}
	}
	return errors.Join(errs...)
}

// Split은 범위를 n개로 나눈다. 앞쪽 조각이 하나씩 더 가질 수 있다.
func Split(r Range, n int) []Range {
	out := make([]Range, n)
	start := r.Start
	for i := range n {
		c := r.Count / int64(n)
		if int64(i) < r.Count%int64(n) {
			c++
		}
		out[i] = Range{start, c}
		start += c
	}
	return out
}

// File은 태스크 하나, 풀 하나의 토큰 파일이다.
type File struct {
	Name string // 예: a-0.json
	Body []byte
}

// Files는 모든 토큰 파일을 만든다. 회원이 없는 풀은 빈 배열 파일을 만들어, k6가 파일이 없어서 실패하지 않게 한다.
func Files(s Signer, r Request, now time.Time) ([]File, error) {
	var files []File
	for _, p := range []struct {
		name string
		r    Range
	}{{"a", r.Members.A}, {"b", r.Members.B}, {"c", r.Members.C}} {
		for i, part := range Split(p.r, r.Tasks) {
			entries, err := sign(s, part, now, r.TTL)
			if err != nil {
				return nil, err
			}
			b, err := json.Marshal(entries)
			if err != nil {
				return nil, err
			}
			files = append(files, File{Name: fmt.Sprintf("%s-%d.json", p.name, i), Body: b})
		}
	}
	return files, nil
}

// sign은 범위의 토큰을 CPU 수만큼 나눠 만든다. 순서는 회원 번호 순서를 지킨다.
func sign(s Signer, r Range, now time.Time, ttl time.Duration) ([]Entry, error) {
	out := make([]Entry, r.Count)
	workers := max(1, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	next := make(chan int64)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				n := r.Start + i
				tok, err := s.Sign(Sub(n), now, ttl)
				if err != nil {
					once.Do(func() { firstErr = err })
					continue
				}
				out[i] = Entry{N: n, Sub: Sub(n), Token: tok}
			}
		}()
	}
	for i := range r.Count {
		next <- i
	}
	close(next)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
