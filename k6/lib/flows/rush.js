// 오픈 러시(풀 A). 몰린 회원 한 명이 상품 상세 → 진입 → 줄 → 접수까지 가고, 그중 일부는 등록 알림을 기다려
// 주문과 결제 시도까지 이어 간다. 결제 승인은 보내지 않는다(토스 paymentKey 가 필요하다).
//
// be (94b27657)
// - 접수 202 의 data.preorderId 는 예약의 공개 토큰이다. 등록 알림의 target.id 와 같은 값이다
// - preorder 가 REGISTERED 로 바꾸면 PREORDER_REGISTERED 알림을 만들고(NotificationRequested), member 가 저장한다
// - 화면은 GET /api/v1/notifications/stream(SSE)으로 안 읽은 수를 받는다. k6 에는 SSE 가 없어
//   GET /api/v1/notifications/unread-count 를 PLAN.rush.notify_interval_seconds 마다 불러 흉내 낸다
// - 알림 목록 GET /api/v1/notifications?size= : 커서 페이지 {items: [{kind, target: {type, id}}], nextCursor}
// - 주문은 REGISTERED 전이면 409 PREORDER_NOT_PAYABLE 이다
import { sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { PLAN, PRODUCT_ID } from '../config.js';
import { data, errorCode, req } from '../http.js';
import { shipTo } from '../fake.js';
import { poolA } from '../users.js';
import { enterAndAccept } from './entry.js';
import { skipped } from './skip.js';

const RUSH = PLAN.rush || { pay_ratio: 0, notify_interval_seconds: 10, notify_timeout_seconds: 480 };
const OPEN_AT_MS = PLAN.open_at ? Date.parse(PLAN.open_at) : null;

// 접수부터 등록 알림을 본 때까지, 판매 오픈부터 결제 시도 응답까지 걸린 시간
const acceptToNotified = new Trend('accept_to_notified', true);
const openToPayment = new Trend('open_to_payment', true);
const outcomes = new Counter('rush_outcome');

function outcome(name) {
  outcomes.add(1, { outcome: name });
}

// 바로 결제할 회원인가. 회원 번호로 고르므로 같은 회원은 늘 같은 쪽이고, 차례로 쓰는 풀 A 에서 고르게 섞인다
function pays(member) {
  return member.n % 100 < Math.round(RUSH.pay_ratio * 100);
}

function unread(member) {
  const d = data(req('GET', '/api/v1/notifications/unread-count', { token: member.token, needBody: true }));
  return d && typeof d.count === 'number' ? d.count : null;
}

/**
 * 예약 preorderId 의 등록 알림이 올 때까지 기다린다. 안 읽은 알림이 있으면 목록을 열어 이 예약의 알림인지 보고,
 * 그 뒤로는 수가 늘었을 때만 다시 연다. 지난 시험의 안 읽은 알림이 남아 있어도 목록을 매번 열지 않는다.
 * 접수 직후의 수를 기준으로 삼지 않는다. 그 사이에 알림이 이미 왔으면 수가 더 늘지 않아 끝내 못 찾는다
 * @returns {boolean} 상한 안에 알림을 받았는가
 */
function waitRegistered(member, preorderId, acceptedAt) {
  let seen = 0;
  const deadline = acceptedAt + RUSH.notify_timeout_seconds * 1000;
  while (Date.now() + RUSH.notify_interval_seconds * 1000 <= deadline) {
    sleep(RUSH.notify_interval_seconds);
    const n = unread(member);
    if (n === null || n <= seen) {
      continue;
    }
    seen = n;
    const page = data(req('GET', '/api/v1/notifications', { query: { size: 5 }, token: member.token, needBody: true }));
    const mine = ((page && page.items) || []).find(
      (x) => x.kind === 'PREORDER_REGISTERED' && x.target && x.target.id === preorderId,
    );
    if (mine) {
      acceptToNotified.add(Date.now() - acceptedAt);
      return true;
    }
  }
  return false;
}

export function rushFlow(setup) {
  const m = poolA();
  if (!m) {
    return skipped('rush', 'no_member');
  }
  // 오픈 순간 상품 상세를 새로 고친다
  req('GET', '/api/v1/products/{id}', { path: { id: PRODUCT_ID } });
  const accepted = enterAndAccept(m, PRODUCT_ID, setup.optionId);
  if (!accepted) {
    return outcome('not_accepted');
  }
  if (!pays(m)) {
    return outcome('left');
  }
  if (!accepted.preorderId) {
    return outcome('no_preorder_id');
  }
  if (!waitRegistered(m, accepted.preorderId, Date.now())) {
    return outcome('notify_timeout');
  }
  // 알림을 눌러 내 예약 상세를 열고, 결제하기를 누른다
  req('GET', '/api/v1/preorders/{id}', { path: { id: accepted.preorderId }, token: m.token, needBody: true });
  const res = req('POST', '/api/v1/orders', {
    token: m.token,
    body: { source: 'PREORDER', preorderId: accepted.preorderId, shipTo: shipTo(m.n) },
    expect: [200, 201],
    needBody: true,
  });
  const order = data(res);
  if (!order || !order.orderId) {
    return outcome(errorCode(res) === 'PREORDER_NOT_PAYABLE' ? 'not_payable' : 'order_failed');
  }
  const paid = req('POST', '/api/v1/orders/{id}/payment-attempts', {
    path: { id: order.orderId },
    token: m.token,
    expect: [201],
    needBody: true,
  });
  if (paid.status !== 201) {
    return outcome('payment_failed');
  }
  if (OPEN_AT_MS !== null) {
    openToPayment.add(Date.now() - OPEN_AT_MS);
  }
  outcome('paid');
}
