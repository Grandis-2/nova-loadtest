// 내 예약 조회(풀 B). be preorder PreorderQueryController (4d5863cb)
// 목록은 커서 페이지 {items, nextCursor}, /active 는 상품마다 많아야 한 건이다. 상세는 목록의 첫 예약을 본다.
import { data, req } from '../http.js';
import { poolB } from '../users.js';
import { skipped } from './skip.js';

export function preordersFlow() {
  const m = poolB();
  if (!m) {
    return skipped('preorders', 'no_member');
  }
  const list = data(req('GET', '/api/v1/preorders', { token: m.token, needBody: true }));
  req('GET', '/api/v1/preorders/active', { token: m.token });
  const first = list && list.items && list.items[0];
  if (first) {
    req('GET', '/api/v1/preorders/{id}', { path: { id: first.preorderId }, token: m.token });
  }
}
