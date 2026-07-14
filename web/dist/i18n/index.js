/**
 * VS AI Proxy - i18n Runtime
 * 零依赖运行时：提供 t() 查找、插值、语言切换。
 */
(function (global) {
  'use strict';

  const STORAGE_KEY = 'vs_ai_proxy_lang';
  const DEFAULT_LANG = 'zh';

  function getLang() {
    try {
      const saved = localStorage.getItem(STORAGE_KEY);
      if (saved && (saved === 'zh' || saved === 'en')) return saved;
    } catch (_) {}
    return DEFAULT_LANG;
  }

  function saveLang(lang) {
    try {
      localStorage.setItem(STORAGE_KEY, lang);
    } catch (_) {}
  }

  function setDocumentLang(lang) {
    document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
  }

  // 运行时翻译表：由各语言文件挂载到 global.LANG 上
  global.LANG = global.LANG || {};

  /**
   * 翻译查找 + 插值
   * @param {string} key
   * @param {...any} args 插值参数，对应消息中的 {0} {1} {2}
   * @returns {string}
   */
  global.t = function (key) {
    const lang = getLang();
    const dict = (global.LANG && global.LANG[lang]) || {};
    let msg = dict[key] || key;
    if (arguments.length > 1) {
      for (let i = 1; i < arguments.length; i++) {
        msg = msg.replace(new RegExp('\\{' + (i - 1) + '\\}', 'g'), String(arguments[i]));
      }
    }
    return msg;
  };

  global.switchLang = function (lang) {
    if (lang !== 'zh' && lang !== 'en') return;
    saveLang(lang);
    setDocumentLang(lang);
    // 刷新页面，让 data-i18n 节点重新渲染
    window.location.reload();
  };

  global.getCurrentLang = getLang;

  // 初始化：遍历所有带 data-i18n 属性的节点并应用翻译
  function applyI18n() {
    const lang = getLang();
    const dict = (global.LANG && global.LANG[lang]) || {};
    setDocumentLang(lang);
    document.querySelectorAll('[data-i18n]').forEach(function (el) {
      const key = el.getAttribute('data-i18n');
      if (dict[key]) {
        // data-i18n 只翻译叶子文本节点，避免 textContent 删除表单控件或图标。
        if (el.childElementCount > 0) return;
        el.textContent = dict[key];
      }
    });
    document.querySelectorAll('[data-i18n-placeholder]').forEach(function (el) {
      const key = el.getAttribute('data-i18n-placeholder');
      if (dict[key]) {
        el.placeholder = dict[key];
      }
    });
    document.querySelectorAll('[data-i18n-aria-label]').forEach(function (el) {
      const key = el.getAttribute('data-i18n-aria-label');
      if (dict[key]) {
        el.setAttribute('aria-label', dict[key]);
      }
    });
    document.querySelectorAll('[data-i18n-alt]').forEach(function (el) {
      const key = el.getAttribute('data-i18n-alt');
      if (dict[key]) {
        el.setAttribute('alt', dict[key]);
      }
    });
    document.querySelectorAll('[data-i18n-title]').forEach(function (el) {
      const key = el.getAttribute('data-i18n-title');
      if (dict[key]) {
        el.setAttribute('title', dict[key]);
      }
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', applyI18n);
  } else {
    applyI18n();
  }
})(typeof globalThis !== 'undefined' ? globalThis : window);
