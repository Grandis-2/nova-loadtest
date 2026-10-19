// 주문 생성 → 결제 시도(풀 B). 결제 승인은 보내지 않는다.
// be order OrderController, PlaceOrderRequest, PaymentAttemptController (4d5863cb)
// - 예약은 /active 가 아니라 목록의 productId 로 이번 준비 상품만 고른다(지난 시험의 예약이 섞이지 않게)
// - POST /api/v1/orders {source: PREORDER, preorderId, shipTo} : 새로 만들면 201, 같은 예약의 주문이 있으면 200
//   PREORDER 주문은 Idempotency-Key 를 받지 않는다. 예약이 REGISTERED 전이면 409 PREORDER_NOT_PAYABLE
// - POST /api/v1/orders/{orderId}/payment-attempts (본문 없음) : 201. 결제사를 부르지 않는다
import { data, req } from '../http.js';
import { PREP_PRODUCT_ID } from '../config.js';
import { shipTo } from '../fake.js';
import { poolB } from '../users.js';
import { skipped } from './skip.js';

export function orderFlow() {
  const m = poolB();
  if (!m) {
    return skipped('order', 'no_member');
  }
  const list = data(req('GET', '/api/v1/preorders', {
    query: { productId: PREP_PRODUCT_ID, status: 'REGISTERED', size: 1 },
    token: m.token,
    needBody: true,
  }));
  const preorder = list && list.items && list.items[0];
  if (!preorder) {
    return skipped('order', 'no_registered');
  }
  const order = data(req('POST', '/api/v1/orders', {
    token: m.token,
    body: { source: 'PREORDER', preorderId: preorder.preorderId, shipTo: shipTo(m.n) },
    expect: [200, 201],
    needBody: true,
  }));
  if (!order || !order.orderId) {
    return;
  }
  req('POST', '/api/v1/orders/{id}/payment-attempts', {
    path: { id: order.orderId },
    token: m.token,
    expect: [201],
    needBody: true,
  });
}
