import { Counter } from 'k6/metrics';

// 요청을 보내지 않고 넘어간 반복. 준비가 늦었거나 회원을 다 쓴 것을 결과에서 보이게 한다
const skips = new Counter('skipped');

export function skipped(flow, reason) {
  skips.add(1, { flow, reason });
}
