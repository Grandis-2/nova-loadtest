// 사전예약 화면이 부르는 API. 진입, rush, 상품 조회 흐름이 같이 쓴다(fe b406737, be 27cbb895).
//
// - 고객은 사전예약 상품을 홍보(사전예약)로만 찾는다. 상품 목록에는 접수 중일 때만 나온다
//   GET /api/v1/promotions → {items: [{promotionId, opensAt, closesAt, ...}]}: 오늘(한국 날짜)이 기간 안인 홍보
//   GET /api/v1/promotions/{id} → {products: [{productId, saleMode, preorderStatus, ...}]}: 묶인 상품 하나가 모델 하나
//   상세 화면은 첫 사전예약 상품의 배송 차수를 함께 부른다(없으면 404)
// - "예약하기"를 누르면 대기열부터 서고, 입장한 뒤 상품 상세(GET /api/v1/products/{id})와 배송 차수를 보고 접수한다
// - 접수 202 뒤에는 예약 상세 GET /api/v1/preorders/{id} 를 1초마다 최대 5초 묻는다. REGISTERED 면 결제 화면으로 간다
import { sleep } from 'k6';
import { data, req } from '../http.js';

const REGISTER_POLLS = 5;

function shipmentBatches(productId) {
  req('GET', '/api/v1/preorders/products/{id}/shipment-batches', { path: { id: productId }, expect: [200, 404] });
}

/** 사전예약 상세 화면. 묶인 상품을 돌려준다 */
export function promotionScreen(promotionId) {
  const d = data(req('GET', '/api/v1/promotions/{id}', { path: { id: promotionId }, needBody: true }));
  const products = (d && d.products) || [];
  const first = products.find((p) => p.saleMode === 'PREORDER');
  if (first) {
    shipmentBatches(first.productId);
  }
  return products;
}

/** 상품 상세 화면. 사전예약은 배송 차수를, 일반 판매는 리뷰를 함께 부른다 */
export function productScreen(productId, saleMode) {
  req('GET', '/api/v1/products/{id}', { path: { id: productId } });
  if (saleMode === 'PREORDER') {
    shipmentBatches(productId);
  } else {
    req('GET', '/api/v1/products/{id}/reviews', { path: { id: productId }, query: { page: 0, size: 100 } });
  }
}

/**
 * 접수 뒤 등록을 잠깐 기다린다(화면은 1초마다 최대 5초).
 * @returns {boolean} 그 안에 REGISTERED 가 됐는가. 아니면 화면은 접수 결과로 가고 등록 알림을 기다린다
 */
export function waitRegistration(token, preorderId) {
  for (let i = 0; i < REGISTER_POLLS; i++) {
    sleep(1);
    const p = data(req('GET', '/api/v1/preorders/{id}', { path: { id: preorderId }, token, needBody: true }));
    if (p && p.status === 'REGISTERED') {
      return true;
    }
  }
  return false;
}

/**
 * 시작할 때 한 번: 시험 상품이 지금 보이는 홍보 중 어디에 묶였는지(promotionOf, 소문자 상품 ID → 홍보 ID) 모은다.
 * 상품 조회 흐름의 사전예약 메뉴는 화면처럼 목록을 매번 새로 부른다
 */
export function promotionSetup() {
  const list = data(req('GET', '/api/v1/promotions', { needBody: true }));
  const promotionOf = {};
  for (const id of ((list && list.items) || []).map((x) => x.promotionId)) {
    const d = data(req('GET', '/api/v1/promotions/{id}', { path: { id }, needBody: true }));
    for (const p of (d && d.products) || []) {
      promotionOf[String(p.productId).toLowerCase()] = id;
    }
  }
  return { promotionOf };
}
