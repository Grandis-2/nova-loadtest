// 진입 → 순번 확인 → 입장권 → 상품 상세 → 접수 → 등록 확인. 준비 구간과 rush 도 상품과 회원만 바꿔 이 흐름을 쓴다.
// 화면 순서는 fe(b406737) 사전예약 상세의 "예약하기"를 따른다: 대기열부터 서고, 입장한 뒤 상품 상세로 가서 접수한다.
//
// be waitingroom QueueController, preorder PreorderController (27cbb895, 4d5863cb 와 같다)
// - POST /api/v1/preorders/queue?productId= : 200 ADMITTED(admissionTicket) 또는 202 WAITING(queueToken, Retry-After)
// - GET  /api/v1/preorders/queue?productId= + Queue-Token : 200. 새 queueToken 이 오면 그 값으로 바꾼다
//   CLOSED: NOT_IN_QUEUE 면 다시 진입하고, SALE_CLOSED 면 끝낸다
//   화면은 줄이 가득 차면(QUEUE_FULL) 10초, 일시 장애(503)면 2초 뒤 다시 진입한다. 횟수 제한은 없다
// - POST /api/v1/preorders?productId= + Idempotency-Key(8~64자) + X-Admission-Ticket, 본문 {productId, optionId} : 202
import { check, sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { TEST_ID } from '../config.js';
import { data, errorCode, header, req } from '../http.js';
import { productScreen, waitRegistration } from './screens.js';

const QUEUE = '/api/v1/preorders/queue';
// 진입부터 접수 응답까지 걸린 시간. true 로 시간 값임을 밝혀 단위가 ms 로 나가게 한다
const entryToAccept = new Trend('entry_to_accept', true);
const outcomes = new Counter('entry_outcome');

// 서버가 주는 확인 간격을 그대로 따른다. 늦으면 서버가 이탈자로 보고 줄에서 뺀다. 다시 진입도 이 수에 든다
const MAX_POLLS = 600;
const QUEUE_FULL_RETRY = 10;
const UNAVAILABLE_RETRY = 2;

function enter(productId, token) {
  return req('POST', QUEUE, { query: { productId }, token, expect: [200, 202], needBody: true });
}

/**
 * 회원 member 로 상품 productId 의 옵션 optionId 를 접수한다. 접수한 뒤 등록을 잠깐 기다린다(화면과 같다).
 * waitRegister 가 false 면 기다리지 않는다(준비 구간은 register_wait 동안 따로 기다린다).
 * @returns {object|null} 접수 응답의 data(preorderId 등)에 registered(5초 안에 REGISTERED 였는가), acceptedAt,
 *   registeredAt 을 더한 것.
 *   접수하지 못했으면 null
 */
export function enterAndAccept(member, productId, optionId, waitRegister = true) {
  const started = Date.now();
  let res = enter(productId, member.token);
  // SharedArray 의 회원 객체는 읽기 전용이라 대기 토큰은 따로 든다
  let queueToken = null;

  for (let i = 0; i < MAX_POLLS; i++) {
    const body = data(res);
    const status = body && body.status;
    if (res.status === 200 && status === 'ADMITTED') {
      // 입장하면 상품 상세로 가서 옵션을 고른다
      productScreen(productId, 'PREORDER');
      return accept(member, productId, optionId, body.admissionTicket, started, waitRegister);
    }
    if (status === 'WAITING') {
      queueToken = body.queueToken || queueToken;
      const wait = Number(header(res, 'Retry-After') || '1');
      sleep(Math.max(1, wait));
      res = req('GET', QUEUE, {
        query: { productId },
        token: member.token,
        headers: { 'Queue-Token': queueToken },
        expect: [200],
        needBody: true,
      });
      const next = data(res);
      if (next && next.queueToken) {
        queueToken = next.queueToken;
      }
      continue;
    }
    if (status === 'CLOSED' && body.reason === 'NOT_IN_QUEUE') {
      queueToken = null;
      res = enter(productId, member.token);
      continue;
    }
    const code = errorCode(res);
    if (code === 'QUEUE_FULL' || res.status === 503) {
      sleep(code === 'QUEUE_FULL' ? QUEUE_FULL_RETRY : UNAVAILABLE_RETRY);
      queueToken = null;
      res = enter(productId, member.token);
      continue;
    }
    // SALE_CLOSED, SALE_NOT_OPEN 같은 오류는 http.js 가 error_code 로 세었다
    outcomes.add(1, { outcome: status === 'CLOSED' ? `closed_${body.reason}` : 'rejected' });
    return null;
  }
  outcomes.add(1, { outcome: 'poll_limit' });
  return null;
}

function accept(member, productId, optionId, ticket, started, waitRegister) {
  const res = req('POST', '/api/v1/preorders', {
    query: { productId },
    token: member.token,
    headers: {
      // 같은 회원이 다시 보내도 중복 접수가 생기지 않는다(8~64자)
      'Idempotency-Key': `${TEST_ID}-${member.n}`,
      'X-Admission-Ticket': ticket,
    },
    body: { productId, optionId },
    expect: [202],
    needBody: true,
  });
  const ok = check(res, { 'accept 202': (r) => r.status === 202 });
  if (!ok) {
    outcomes.add(1, { outcome: 'accept_failed' });
    return null;
  }
  const acceptedAt = Date.now();
  entryToAccept.add(acceptedAt - started);
  const accepted = data(res) || {};
  const registered = waitRegister && accepted.preorderId ? waitRegistration(member.token, accepted.preorderId) : false;
  outcomes.add(1, { outcome: registered ? 'registered' : 'accepted' });
  // acceptedAt 은 접수 응답을 받은 때, registeredAt 은 등록 확인에서 REGISTERED 를 본 때다(못 봤으면 null)
  return Object.assign({}, accepted, { registered, acceptedAt, registeredAt: registered ? Date.now() : null });
}
