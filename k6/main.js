// 부하 시험 진입점. PLAN_JSON(계획 계산 결과)의 시나리오를 그대로 k6 시나리오로 만든다.
// 흐름 하나가 k6 시나리오 하나이고, 실행기는 도착률 기반만 쓴다.
import { data, req } from './lib/http.js';
import { PLAN, PREP_PRODUCT_ID, PRODUCT_ID, SUMMARY_PATH, TEST_ID } from './lib/config.js';
import { catalogFlow, catalogSetup } from './lib/flows/catalog.js';
import { cancelFlow } from './lib/flows/cancel.js';
import { entryFlow, prepareFlow } from './lib/flows/entry_flows.js';
import { orderFlow } from './lib/flows/order.js';
import { preordersFlow } from './lib/flows/preorders.js';
import { rushFlow } from './lib/flows/rush.js';
import { promotionSetup } from './lib/flows/screens.js';

function scenarios() {
  const out = {};
  for (const s of PLAN.scenarios) {
    const { name, flow, ...k6 } = s;
    out[name] = Object.assign({}, k6, { exec: flow });
  }
  return out;
}

export const options = {
  scenarios: scenarios(),
  // 본문은 기본으로 버리고, 필요한 요청만 http.js 에서 받는다
  discardResponseBodies: true,
  // url 은 넣지 않는다. 경로 템플릿은 name 태그로 들어간다
  systemTags: ['status', 'method', 'name', 'scenario', 'expected_response'],
  userAgent: `nova-loadtest/${TEST_ID}`,
  tags: { test_id: TEST_ID },
  setupTimeout: '2m',
  // 태스크마다 따로 판단하는 안전장치다. 합격 여부는 컨트롤러가 Mimir 로 정한다.
  // 누적 비율이라 오래 정상으로 돈 뒤에는 늦게 걸린다(docs/design.md "결과 합치기와 판정")
  thresholds: {
    http_req_failed: [{ threshold: `rate<${PLAN.abort_error_rate / 100}`, abortOnFail: true, delayAbortEval: '1m' }],
  },
};

// 상품 상세에서 판매 중인 첫 옵션을 고른다. 접수 본문의 optionId 는 상세의 variants[].variantId 와 같은 값이다
function activeOption(productId) {
  if (!productId) {
    return null;
  }
  const p = data(req('GET', '/api/v1/products/{id}', { path: { id: productId }, needBody: true }));
  const v = p && (p.variants || []).find((x) => x.status === 'ACTIVE');
  if (!v) {
    throw new Error(`시험 상품 ${productId}에 판매 중인 옵션이 없다`);
  }
  return v.variantId;
}

// setup 의 반환값은 요약에 그대로 들어간다. 토큰이나 헤더 값을 넣지 않고 공개 값만 둔다
export function setup() {
  const flows = new Set(PLAN.flows);
  const list = data(req('GET', '/api/v1/products', { query: { page: 0, size: 100 }, needBody: true }));
  const products = (list && list.items) || [];
  // 진입(준비 포함)만 사전예약 상세에서 시작한다
  const promos = flows.has('entry') || PLAN.prep_product_id ? promotionSetup() : { promotionOf: {} };
  return Object.assign(flows.has('catalog') ? catalogSetup(products) : {}, {
    optionId: flows.has('entry') ? activeOption(PRODUCT_ID) : null,
    prepOptionId: PLAN.prep_product_id ? activeOption(PREP_PRODUCT_ID) : null,
    promotionId: flows.has('entry') ? promotionOf(promos, PRODUCT_ID) : null,
    prepPromotionId: PLAN.prep_product_id ? promotionOf(promos, PREP_PRODUCT_ID) : null,
  });
}

// 시험 상품이 묶인 사전예약(홍보). 고객은 사전예약 상세에서만 "예약하기"를 누를 수 있으므로, 묶이지 않았으면 시작하지 않는다
function promotionOf(promos, productId) {
  const id = promos.promotionOf[String(productId).toLowerCase()];
  if (!id) {
    throw new Error(`시험 상품 ${productId}이 지금 보이는 사전예약(홍보)에 묶여 있지 않다. testproduct 로 만들거나 관리자 화면에서 묶는다`);
  }
  return id;
}

export function catalog(setupData) {
  catalogFlow(setupData);
}

export function preorders() {
  preordersFlow();
}

export function entry(setupData) {
  entryFlow(setupData);
}

export function order() {
  orderFlow();
}

export function cancel() {
  cancelFlow();
}

export function prepare(setupData) {
  prepareFlow(setupData);
}

export function rush(setupData) {
  rushFlow(setupData);
}

// 요약은 JSON 하나만 쓴다. k6-runner 가 종료 코드와 함께 S3 results/<시험 ID>/k6-<태스크>.json 에 올린다
export function handleSummary(summary) {
  return {
    [SUMMARY_PATH]: JSON.stringify(summary),
    stdout: `요약을 ${SUMMARY_PATH}에 썼다\n`,
  };
}
