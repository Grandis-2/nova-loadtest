// 상품 조회. 토큰이 필요 없다. 프론트(fe b406737)의 화면이 부르는 순서를 따른다. 비율은 PLAN.catalog(scenarios.yml catalog_paths)다.
//
// - 홈: 인기 상품 ?size=10, 카테고리 트리, 대분류 줄마다 ?categoryId=(기본 size 20)
// - 카테고리 목록 ?categoryId=&page=&size=12. 쪽 번호를 눌러 넘긴다. hasNext 가 false 면 더 넘기지 않는다
// - 검색창 ?q=&size=8 → 검색 결과 ?q=&size=12
// - 사전예약 메뉴: 사전예약 목록 GET /api/v1/promotions → 사전예약 상세(첫 사전예약 상품의 배송 차수). 모델을 눌러
//   "예약하기"는 로그인이 필요하므로 여기서 끝난다(접수는 진입 흐름이 한다). 지금 보이는 사전예약이 없으면 목록에서 끝난다
// - 상품을 누르면 상세와 함께, 사전예약은 배송 차수(preorder), 일반 판매는 리뷰 ?page=0&size=100 을 부른다
// 정렬(sort)은 화면이 보내지 않아 쓰지 않는다. be catalog ProductController, PromotionController, ReviewController,
// preorder ProductShipmentBatchController (27cbb895)
import { PLAN } from '../config.js';
import { data, req } from '../http.js';
import { productScreen, promotionScreen } from './screens.js';

const P = PLAN.catalog || { home_rows: 0, category: 0, search: 0, preorder: 0, next_page: 0, max_pages: 1, click: 0 };
const LIST = '/api/v1/products';

function pick(xs) {
  return xs[Math.floor(Math.random() * xs.length)];
}

// 목록 한 쪽. 본문을 받아 다음 쪽 여부와 누를 상품을 고른다
function list(query) {
  const page = data(req('GET', LIST, { query, needBody: true }));
  return { items: (page && page.items) || [], hasNext: Boolean(page && page.hasNext) };
}

function categoryPages(categoryId) {
  let shown = list({ categoryId, page: 0, size: 12 });
  for (let page = 1; page < P.max_pages && shown.hasNext && Math.random() < P.next_page; page++) {
    shown = list({ categoryId, page, size: 12 });
  }
  return shown.items;
}

function search(keyword) {
  // 검색창의 미리보기는 고르지 않으므로 본문을 받지 않는다
  req('GET', LIST, { query: { q: keyword, size: 8 } });
  return list({ q: keyword, size: 12 }).items;
}

function preorderMenu() {
  const promos = data(req('GET', '/api/v1/promotions', { needBody: true }));
  const items = (promos && promos.items) || [];
  if (items.length > 0) {
    promotionScreen(pick(items).promotionId);
  }
}

export function catalogFlow(setup) {
  const categories = setup.categories || { top: [], all: [] };
  const keywords = setup.keywords || [];

  // 홈. 대분류 줄은 고르지 않으므로 본문을 받지 않는다
  let shown = list({ size: 10 }).items;
  req('GET', '/api/v1/categories');
  for (const id of categories.top.slice(0, P.home_rows)) {
    req('GET', LIST, { query: { categoryId: id } });
  }

  const r = Math.random();
  if (r < P.category && categories.all.length > 0) {
    shown = categoryPages(pick(categories.all));
  } else if (r >= P.category && r < P.category + P.search && keywords.length > 0) {
    shown = search(pick(keywords));
  } else if (r >= P.category + P.search && r < P.category + P.search + P.preorder) {
    // 사전예약 상세에서 끝난다. 상품 목록을 보지 않았으므로 누를 상품이 없다
    preorderMenu();
    return;
  }

  if (shown.length > 0 && Math.random() < P.click) {
    const p = pick(shown);
    productScreen(p.productId, p.saleMode);
  }
}

/**
 * setup 에서 한 번 준비한다. 카테고리는 트리의 대분류와 모든 카테고리(프론트는 대분류와 하위 분류를 모두 목록에 쓴다),
 * 검색어는 상품 목록의 모델명과 상품명 첫 단어다. 결과가 0건인 검색만 되풀이하지 않게 실제 상품에서 뽑는다.
 */
export function catalogSetup(products) {
  const tree = data(req('GET', '/api/v1/categories', { needBody: true }));
  const top = [];
  const all = [];
  const walk = (nodes, depth) => {
    for (const n of nodes || []) {
      if (depth === 0) {
        top.push(n.categoryId);
      }
      all.push(n.categoryId);
      walk(n.children, depth + 1);
    }
  };
  walk(tree && tree.items, 0);
  // 준비하지 못하면 그 경로를 말없이 건너뛰어 계획과 다른 부하가 나간다. 시작하기 전에 멈춘다
  if ((P.category > 0 || P.home_rows > 0) && all.length === 0) {
    throw new Error('카테고리 트리를 받지 못했다(GET /api/v1/categories). 카테고리 목록과 홈의 대분류 줄을 보낼 수 없다');
  }
  const words = new Set();
  for (const p of products) {
    if (p.modelName) {
      words.add(String(p.modelName).slice(0, 50));
    }
    const first = String(p.title || '').trim().split(/\s+/)[0];
    if (first && !first.startsWith('[')) {
      words.add(first.slice(0, 50));
    }
  }
  if (P.search > 0 && words.size === 0) {
    throw new Error('검색어를 뽑을 상품이 없다(GET /api/v1/products). 검색을 보낼 수 없다');
  }
  return { categories: { top, all }, keywords: [...words] };
}
