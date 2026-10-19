// 환경 변수를 읽고 검사한다. 값의 뜻은 docs/implementation-plan.md 2.2 에 있다.
// k6-runner 가 컨트롤러에게 받은 값을 그대로 넘긴다.

function required(name) {
  const v = __ENV[name];
  if (!v) {
    throw new Error(`환경 변수 ${name}가 없다`);
  }
  return v;
}

export const TEST_ID = required('TEST_ID');
export const TASK_INDEX = Number(__ENV.TASK_INDEX || '0');
export const BASE_URL = required('BASE_URL').replace(/\/+$/, '');
export const PLAN = JSON.parse(required('PLAN_JSON'));
export const PRODUCT_ID = __ENV.PRODUCT_ID || PLAN.product_id || '';
export const PREP_PRODUCT_ID = __ENV.PREP_PRODUCT_ID || PLAN.prep_product_id || '';
export const TOKENS_DIR = __ENV.TOKENS_DIR || '/tmp/tokens';
export const SUMMARY_PATH = __ENV.SUMMARY_PATH || '/tmp/summary.json';
// 부하 시험 헤더 값. 작업 정의 secrets 로 들어오고, 시나리오와 관계없이 늘 붙인다
export const LOADTEST_HEADER = __ENV.LOADTEST_HEADER || '';

// 로컬에서 운영 주소로 보내지 않게 막는다(구현 계획 7.4)
if (__ENV.LOCAL === '1' && /grandis-nova\.link/.test(BASE_URL)) {
  throw new Error(`LOCAL=1 인데 BASE_URL 이 운영 주소다: ${BASE_URL}`);
}
