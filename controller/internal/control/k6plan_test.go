package control

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/plan"
)

// k6 스크립트가 PLAN.<이름> 으로 읽는 값은 모두 k6Plan 에 있어야 한다. 빠지면 k6 는 오류 없이 기본값으로 돈다
// (rush 의 결제 비율이 0 이 되어 아무도 결제하지 않았다. PR #12 리뷰)
func TestK6PlanHasWhatK6Reads(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.New(cfg, plan.Input{TestID: testID, Scenario: "rush", ProductID: product,
		OpenAt: t0.Add(20 * time.Minute).Format(time.RFC3339), Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(k6Plan(p)), &got); err != nil {
		t.Fatal(err)
	}

	re := regexp.MustCompile(`PLAN\.(\w+)`)
	read := map[string]string{}
	err = filepath.WalkDir("../../../k6", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		b, err := os.ReadFile(path)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			read[m[1]] = path
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(read) == 0 {
		t.Fatal("k6 스크립트에서 PLAN 을 읽는 곳을 찾지 못했다")
	}
	for name, path := range read {
		if _, ok := got[name]; !ok {
			t.Errorf("%s 가 PLAN.%s 를 읽는데 k6Plan 에 없다", path, name)
		}
	}

	var rush plan.Rush
	if json.Unmarshal(got["rush"], &rush) != nil || rush != *p.Rush || string(got["open_at"]) != `"`+p.OpenAt+`"` {
		t.Fatalf("rush 값과 open_at 이 그대로 가야 한다: %s %s", got["rush"], got["open_at"])
	}
}
