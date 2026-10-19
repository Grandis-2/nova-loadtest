// 모든 요청은 이 파일의 req() 로만 보낸다. 다른 파일에서 k6/http 를 바로 부르면 CI 가 실패한다.
//
// - 경로는 템플릿과 값을 따로 받는다. 템플릿을 그대로 tags.name 에 넣어 ID 마다 시계열이 생기지 않게 한다.
// - 템플릿이 아래 목록에 없으면 예외를 던진다. 목록을 늘리면 시계열 계산(scenarios.yml series.names)도 다시 본다.
// - 기대 상태(responseCallback)는 요청마다 준다. 대기열 202 처럼 정상인 응답을 실패로 세지 않게 한다.
// - 오류 응답은 본문의 error.code 로 error_code 카운터를 올린다. 값이 be 의 오류 코드 목록이라 시계열이 늘지 않는다.
import http from 'k6/http';
import { Counter } from 'k6/metrics';
import { BASE_URL, LOADTEST_HEADER } from './config.js';

export const TEMPLATES = Object.freeze([
  '/api/v1/products',
  '/api/v1/products/{id}',
  '/api/v1/categories',
  '/api/v1/preorders/queue',
  '/api/v1/preorders',
  '/api/v1/preorders/active',
  '/api/v1/preorders/{id}',
  '/api/v1/preorders/{id}/cancel',
  '/api/v1/orders',
  '/api/v1/orders/{id}/payment-attempts',
]);

const allowed = new Set(TEMPLATES);
const callbacks = new Map();
const errorCodes = new Counter('error_code');

function callback(expect) {
  const key = expect.join(',');
  if (!callbacks.has(key)) {
    callbacks.set(key, http.expectedStatuses(...expect));
  }
  return callbacks.get(key);
}

function buildURL(template, path, query) {
  let url = BASE_URL + template.replace(/\{(\w+)\}/g, (_, k) => {
    if (path[k] === undefined || path[k] === null || path[k] === '') {
      throw new Error(`${template}의 {${k}} 값이 없다`);
    }
    return encodeURIComponent(path[k]);
  });
  const qs = Object.entries(query || {})
    .filter(([, v]) => v !== undefined && v !== null && v !== '')
    .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`)
    .join('&');
  return qs ? `${url}?${qs}` : url;
}

/**
 * 요청을 보낸다.
 * @param {string} method
 * @param {string} template TEMPLATES 중 하나
 * @param {object} o path, query, token, body, headers, expect(기대 상태 목록), needBody(본문을 받을지)
 */
export function req(method, template, o = {}) {
  if (!allowed.has(template)) {
    throw new Error(`요청 템플릿 ${template}가 목록에 없다(k6/lib/http.js TEMPLATES)`);
  }
  const headers = Object.assign({}, o.headers);
  if (o.token) {
    headers.Authorization = `Bearer ${o.token}`;
  }
  if (LOADTEST_HEADER) {
    headers['x-nova-loadtest'] = LOADTEST_HEADER;
  }
  let body = null;
  if (o.body !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(o.body);
  }
  const res = http.request(method, buildURL(template, o.path || {}, o.query), body, {
    headers,
    tags: { name: template },
    responseCallback: callback(o.expect || [200]),
    // 본문은 기본으로 버린다(discardResponseBodies). 오류 코드를 세려면 본문이 필요하므로 needBody 가 아니어도
    // 오류만 세고 싶은 요청은 needBody 를 켠다
    responseType: o.needBody ? 'text' : 'none',
  });
  if (res.status >= 400) {
    const code = errorCode(res);
    errorCodes.add(1, { code: code || `HTTP_${res.status}`, name: template });
  }
  return res;
}

/** 응답 봉투 {success, data, error, ...}의 data. 본문을 받지 않았거나 JSON 이 아니면 null. */
export function data(res) {
  try {
    return JSON.parse(res.body).data;
  } catch (e) {
    return null;
  }
}

/** 응답 봉투의 error.code. 없으면 null. */
export function errorCode(res) {
  try {
    const e = JSON.parse(res.body).error;
    return e && e.code ? String(e.code) : null;
  } catch (e) {
    return null;
  }
}

/** 헤더 값(대소문자 무관). */
export function header(res, name) {
  const want = name.toLowerCase();
  for (const k of Object.keys(res.headers || {})) {
    if (k.toLowerCase() === want) {
      return res.headers[k];
    }
  }
  return null;
}
