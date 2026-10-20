// 내 예약 조회(풀 B). be preorder PreorderQueryController (27cbb895, 4d5863cb 와 같다)
// 화면(fe b406737 useReservations)은 목록을 size 100 으로 한 번 부르고 /active 는 부르지 않는다. 상세는 목록의 첫 예약을 본다.
import { data, req } from '../http.js';
import { poolB } from '../users.js';
import { skipped } from './skip.js';

export function preordersFlow() {
  const m = poolB();
  if (!m) {
    return skipped('preorders', 'no_member');
  }
  const list = data(req('GET', '/api/v1/preorders', { query: { size: 100 }, token: m.token, needBody: true }));
  const first = list && list.items && list.items[0];
  if (first) {
    req('GET', '/api/v1/preorders/{id}', { path: { id: first.preorderId }, token: m.token });
  }
}
