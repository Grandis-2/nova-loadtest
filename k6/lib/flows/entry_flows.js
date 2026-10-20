// 진입 흐름 두 가지. 본 시험의 진입(풀 A, 시험 상품)과 준비 구간(풀 B → C, 준비 상품)이다.
// 둘 다 사전예약 상세 화면에서 "예약하기"를 누르는 것부터 시작한다(fe b406737).
import { PREP_PRODUCT_ID, PRODUCT_ID } from '../config.js';
import { poolA, prepareMember } from '../users.js';
import { enterAndAccept } from './entry.js';
import { promotionScreen } from './screens.js';
import { skipped } from './skip.js';

export function entryFlow(setup) {
  const m = poolA();
  if (!m) {
    return skipped('entry', 'no_member');
  }
  promotionScreen(setup.promotionId);
  enterAndAccept(m, PRODUCT_ID, setup.optionId);
}

/** 준비 구간: 풀 B, C 가 준비 상품에 예약을 만들어 둔다. 조회, 결제, 취소 흐름이 이 예약을 쓴다. */
export function prepareFlow(setup) {
  const m = prepareMember();
  if (!m) {
    return skipped('prepare', 'no_member');
  }
  promotionScreen(setup.prepPromotionId);
  // 등록은 준비 구간 뒤 register_wait 동안 기다리므로 등록 확인을 보내지 않는다
  enterAndAccept(m, PREP_PRODUCT_ID, setup.prepOptionId, false);
}
