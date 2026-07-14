'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const sources = [
  'web/dist/i18n/index.js',
  'web/dist/i18n/zh.js',
  'web/dist/i18n/en.js'
].map((path) => ({ path, source: fs.readFileSync(path, 'utf8') }));

function createElement(attributes, initialText, childElementCount = 0) {
  let text = initialText;
  const state = { ...attributes };
  return {
    childElementCount,
    getAttribute(name) {
      return state[name] || null;
    },
    setAttribute(name, value) {
      state[name] = value;
    },
    get textContent() {
      return text;
    },
    set textContent(value) {
      if (childElementCount > 0) {
        throw new Error('i18n runtime attempted to replace an element containing controls');
      }
      text = value;
    },
    get placeholder() {
      return state.placeholder || '';
    },
    set placeholder(value) {
      state.placeholder = value;
    },
    state
  };
}

function runLanguageCase(language, expected) {
  const leaf = createElement({ 'data-i18n': 'nav.providers' }, 'original leaf');
  const parent = createElement({ 'data-i18n': 'nav.providers' }, 'preserve child control', 1);
  const placeholder = createElement({ 'data-i18n-placeholder': 'logs.filter.search.placeholder' }, '');
  const aria = createElement({ 'data-i18n-aria-label': 'app.title' }, '');
  const alt = createElement({ 'data-i18n-alt': 'faq.vsGuide.screenshot1' }, '');
  const title = createElement({ 'data-i18n-title': 'providers.form.showKey' }, '');
  const elements = {
    '[data-i18n]': [leaf, parent],
    '[data-i18n-placeholder]': [placeholder],
    '[data-i18n-aria-label]': [aria],
    '[data-i18n-alt]': [alt],
    '[data-i18n-title]': [title]
  };

  let storedLanguage = language;
  let domReady;
  let reloadCount = 0;
  const document = {
    readyState: 'loading',
    documentElement: { lang: 'zh-CN' },
    querySelectorAll(selector) {
      return elements[selector] || [];
    },
    addEventListener(event, listener) {
      if (event === 'DOMContentLoaded') domReady = listener;
    }
  };
  const context = vm.createContext({
    console,
    document,
    localStorage: {
      getItem() {
        return storedLanguage;
      },
      setItem(_key, value) {
        storedLanguage = value;
      }
    },
    window: {
      location: {
        reload() {
          reloadCount++;
        }
      }
    }
  });

  for (const source of sources) {
    vm.runInContext(source.source, context, { filename: source.path });
  }
  assert.equal(typeof domReady, 'function');
  domReady();

  assert.equal(leaf.textContent, expected.providers);
  assert.equal(parent.textContent, 'preserve child control');
  assert.equal(placeholder.placeholder, expected.searchPlaceholder);
  assert.equal(aria.state['aria-label'], 'VS AI Proxy');
  assert.equal(alt.state.alt, expected.screenshotAlt);
  assert.equal(title.state.title, expected.showKey);
  assert.equal(document.documentElement.lang, expected.documentLang);
  assert.equal(context.t('logs.pageInfo', 1, 2, 3), expected.pageInfo);

  const nextLanguage = language === 'zh' ? 'en' : 'zh';
  context.switchLang(nextLanguage);
  assert.equal(storedLanguage, nextLanguage);
  assert.equal(document.documentElement.lang, nextLanguage === 'zh' ? 'zh-CN' : 'en');
  assert.equal(reloadCount, 1);
}

runLanguageCase('zh', {
  providers: '提供商',
  searchPlaceholder: '搜索路径 / 模型 / 错误 / 请求ID',
  screenshotAlt: 'Visual Studio Copilot / BYOM 设置入口',
  showKey: '显示 API Key',
  documentLang: 'zh-CN',
  pageInfo: '第 1/2 页（共 3 条）'
});

runLanguageCase('en', {
  providers: 'Providers',
  searchPlaceholder: 'Search path / model / error / request ID',
  screenshotAlt: 'Visual Studio Copilot / BYOM settings entry',
  showKey: 'Show API Key',
  documentLang: 'en',
  pageInfo: 'Page 1/2 (3 total)'
});

console.log('I18N_RUNTIME_TEST_OK');
