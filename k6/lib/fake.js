// 자연스러운 한글 가짜 데이터. 주문 배송지(shipTo)와 취소 사유에 쓴다.
//
// - 회원 번호를 시드로 써서 같은 회원은 늘 같은 값을 얻는다. 시험을 다시 돌려도 결과를 비교할 수 있다.
// - 전화번호는 개통되지 않는 형식(010-0xxx-xxxx, 가운데 자리가 0으로 시작)이라 실제 번호와 겹치지 않는다.
// - be 의 배송지 검사는 길이만 본다: 이름 50자, 전화 20자, 우편번호 10자, 주소 200자, 상세 주소는 선택
//   (order PlaceOrderRequest.ShipTo, be 4d5863cb).

const SURNAMES = ['김', '이', '박', '최', '정', '강', '조', '윤', '장', '임', '한', '오', '서', '신', '권', '황', '안', '송', '류', '홍'];
const GIVEN = [
  '민준', '서연', '도윤', '하은', '시우', '지우', '예준', '서윤', '주원', '지민',
  '하준', '수아', '지호', '지아', '준우', '채원', '건우', '다은', '현우', '유나',
  '우진', '소율', '선우', '예린', '연우', '하린', '유준', '시아', '정우', '나윤',
];

// 시군구와 도로 이름, 우편번호 범위. 우편번호는 지역별 대략적인 범위라 실제 주소와 맞지 않을 수 있다(형식만 맞으면 된다)
const AREAS = [
  { sido: '서울특별시', sigungu: '강남구', roads: ['테헤란로', '도산대로', '선릉로', '역삼로'], zip: [6000, 6399] },
  { sido: '서울특별시', sigungu: '마포구', roads: ['월드컵북로', '양화로', '독막로', '마포대로'], zip: [3900, 4199] },
  { sido: '서울특별시', sigungu: '송파구', roads: ['올림픽로', '송파대로', '석촌호수로', '가락로'], zip: [5500, 5899] },
  { sido: '서울특별시', sigungu: '성동구', roads: ['왕십리로', '성수일로', '아차산로', '뚝섬로'], zip: [4700, 4899] },
  { sido: '서울특별시', sigungu: '관악구', roads: ['관악로', '남부순환로', '봉천로', '신림로'], zip: [8700, 8899] },
  { sido: '경기도', sigungu: '성남시 분당구', roads: ['판교역로', '정자일로', '황새울로', '불정로'], zip: [13400, 13699] },
  { sido: '경기도', sigungu: '수원시 영통구', roads: ['광교중앙로', '봉영로', '매영로', '영통로'], zip: [16500, 16799] },
  { sido: '경기도', sigungu: '고양시 일산동구', roads: ['중앙로', '정발산로', '일산로', '장백로'], zip: [10300, 10499] },
  { sido: '인천광역시', sigungu: '연수구', roads: ['컨벤시아대로', '송도과학로', '아트센터대로', '센트럴로'], zip: [21900, 22099] },
  { sido: '부산광역시', sigungu: '해운대구', roads: ['해운대로', '센텀중앙로', '달맞이길', '좌동순환로'], zip: [48000, 48199] },
  { sido: '대구광역시', sigungu: '수성구', roads: ['달구벌대로', '동대구로', '수성로', '들안로'], zip: [42000, 42299] },
  { sido: '대전광역시', sigungu: '유성구', roads: ['대학로', '도안대로', '계룡로', '노은로'], zip: [34100, 34299] },
  { sido: '광주광역시', sigungu: '서구', roads: ['상무중앙로', '내방로', '죽봉대로', '화운로'], zip: [61900, 62099] },
];

const APARTMENTS = ['래미안', '자이', '푸르지오', '힐스테이트', '아이파크', 'e편한세상', '롯데캐슬', '더샵'];
const CANCEL_REASONS = [
  '단순 변심으로 취소합니다.',
  '다른 색상으로 다시 예약하려고 합니다.',
  '배송 일정이 맞지 않아 취소합니다.',
  '결제 수단을 바꾸려고 합니다.',
  '가족이 이미 구매해서 취소합니다.',
];

// mulberry32. 작은 시드 PRNG 라 같은 시드면 같은 수열이 나온다
function rng(seed) {
  let a = (seed * 2654435761) >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function pick(r, list) {
  return list[Math.floor(r() * list.length)];
}

function between(r, lo, hi) {
  return lo + Math.floor(r() * (hi - lo + 1));
}

function pad(n, width) {
  return String(n).padStart(width, '0');
}

/** 회원 번호 n 의 이름. 예: 김서연 */
export function name(n) {
  const r = rng(n);
  return pick(r, SURNAMES) + pick(r, GIVEN);
}

/** 회원 번호 n 의 전화번호. 개통되지 않는 형식이다. 예: 010-0123-4567 */
export function phone(n) {
  const r = rng(n * 7 + 1);
  return `010-0${pad(between(r, 0, 999), 3)}-${pad(between(r, 0, 9999), 4)}`;
}

/** 회원 번호 n 의 배송지. be 의 shipTo 형식 그대로다. */
export function shipTo(n) {
  const r = rng(n * 13 + 5);
  const area = pick(r, AREAS);
  const road = pick(r, area.roads);
  const line1 = `${area.sido} ${area.sigungu} ${road} ${between(r, 1, 420)}`;
  let line2;
  const kind = r();
  if (kind < 0.6) {
    line2 = `${pick(r, APARTMENTS)} ${between(r, 101, 125)}동 ${between(r, 1, 25)}${pad(between(r, 1, 4), 2)}호`;
  } else if (kind < 0.85) {
    line2 = `${between(r, 2, 15)}층`;
  } else {
    line2 = null; // 단독 주택은 상세 주소가 없다
  }
  const body = {
    name: name(n),
    phone: phone(n),
    postalCode: pad(between(r, area.zip[0], area.zip[1]), 5),
    line1,
  };
  if (line2) {
    body.line2 = line2;
  }
  return body;
}

/** 회원 번호 n 의 취소 사유. */
export function cancelReason(n) {
  return pick(rng(n * 31 + 7), CANCEL_REASONS);
}
