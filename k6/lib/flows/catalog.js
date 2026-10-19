// 상품 조회. 토큰이 필요 없다(be catalog ProductController, CategoryController, 4d5863cb).
// 목록의 쪽은 앞 10,000개 안에서 시작해야 한다(page × size < 10,000). 앞쪽 몇 쪽만 본다.
import { req } from '../http.js';

const PAGE_SIZE = 20;

export function catalogFlow(setup) {
  const ids = setup.catalogIds || [];
  req('GET', '/api/v1/products', { query: { page: Math.floor(Math.random() * 3), size: PAGE_SIZE } });
  if (ids.length > 0) {
    req('GET', '/api/v1/products/{id}', { path: { id: ids[Math.floor(Math.random() * ids.length)] } });
  }
  req('GET', '/api/v1/categories');
}
