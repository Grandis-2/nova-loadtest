// 시험 회원(토큰)을 고른다. 토큰 발급기가 태스크와 풀마다 파일을 따로 만들어 두었으므로({a,b,c}-<태스크>.json)
// 태스크끼리 회원이 겹치지 않는다. 파일은 SharedArray 로 한 번만 읽어 모든 VU 가 같이 쓴다.
//
// - 풀 A(진입과 접수): 회원 한 명을 시험 하나에서 한 번만 쓴다. 시나리오가 풀을 나눠 쓰면 POOL_FROM, POOL_TO 로 구간을 받는다
// - 풀 B(조회, 결제): 같은 회원을 되풀이해 쓴다
// - 풀 C(취소): 한 번만 쓴다
// - 준비 구간: 풀 B 를 먼저, 이어서 풀 C 를 쓴다
import { SharedArray } from 'k6/data';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';
import { TASK_INDEX, TOKENS_DIR } from './config.js';

function load(pool) {
  return new SharedArray(`pool-${pool}`, () => {
    let text;
    try {
      text = open(`${TOKENS_DIR}/${pool}-${TASK_INDEX}.json`);
    } catch (e) {
      // 상품 조회만 하는 시험에는 토큰 파일이 없다
      return [];
    }
    return JSON.parse(text);
  });
}

const pools = { a: load('a'), b: load('b'), c: load('c') };
const exhausted = new Counter('members_exhausted');

function iteration() {
  return exec.scenario.iterationInInstance;
}

function out(pool) {
  exhausted.add(1, { pool });
  return null;
}

/** 풀 A 의 회원. 이 시나리오에 준 구간을 다 쓰면 null. */
export function poolA() {
  const a = pools.a;
  // 경계를 반올림한다. 앞 시나리오의 끝과 뒤 시나리오의 시작이 같은 값으로 계산되어 겹치거나 비지 않는다
  const from = Math.round(Number(__ENV.POOL_FROM || '0') * a.length);
  const to = Math.round(Number(__ENV.POOL_TO || '1') * a.length);
  const i = from + iteration();
  return i < to ? a[i] : out('a');
}

/** 풀 B 의 회원. 차례로 되풀이한다. */
export function poolB() {
  const b = pools.b;
  return b.length > 0 ? b[iteration() % b.length] : out('b');
}

/** 풀 C 의 회원. 다 쓰면 null. */
export function poolC() {
  const c = pools.c;
  const i = iteration();
  return i < c.length ? c[i] : out('c');
}

/** 준비 구간의 회원. 풀 B 를 먼저, 이어서 풀 C 를 쓴다. */
export function prepareMember() {
  const i = iteration();
  const b = pools.b;
  const c = pools.c;
  if (i < b.length) {
    return b[i];
  }
  return i - b.length < c.length ? c[i - b.length] : out('prepare');
}
