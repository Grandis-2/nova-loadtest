// 예약 취소(풀 C). be preorder PreorderCancelController, CancelRequest (4d5863cb)
// - 본문은 선택({reason}, 500자 이하)이고 Idempotency-Key 는 받지 않는다. 이미 취소됐으면 아무것도 하지 않고 202
// - 예약은 목록의 productId 로 이번 준비 상품만 고른다
import { data, req } from '../http.js';
import { PREP_PRODUCT_ID } from '../config.js';
import { cancelReason } from '../fake.js';
import { poolC } from '../users.js';
import { skipped } from './skip.js';

const CANCELABLE = new Set(['PENDING_SYNC', 'REGISTERED']);

export function cancelFlow() {
  const m = poolC();
  if (!m) {
    return skipped('cancel', 'no_member');
  }
  const list = data(req('GET', '/api/v1/preorders', {
    query: { productId: PREP_PRODUCT_ID },
    token: m.token,
    needBody: true,
  }));
  const preorder = list && list.items && list.items.find((p) => CANCELABLE.has(p.status));
  if (!preorder) {
    return skipped('cancel', 'no_cancelable');
  }
  req('POST', '/api/v1/preorders/{id}/cancel', {
    path: { id: preorder.preorderId },
    token: m.token,
    body: { reason: cancelReason(m.n) },
    expect: [202],
    needBody: true,
  });
}
