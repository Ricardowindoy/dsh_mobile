window.__ModuleLoader__.load({
  id: '@local/dsh-remote-access',
  factory(require) {
    const React = require('react');
    const ReactDOM = require('react-dom');
    const h = React.createElement;

    const NS = 'remoteAccess';
    // API 基址：页面经 relay 的 /d/<deviceId>/ 前缀打开时，根路径 /api/* 不带
    // 节点前缀会被 relay 路由到 default-node（可能不是本页节点）且鉴权上下文
    // 不同（实测根路径 401、前缀路径 200）。因此把当前 URL 的 /d/<id>/ 前缀
    // 拼到 API 路径前，保证请求落在「服务本页的那个节点」上。直连/根路径无前缀。
    const API_BASE = (() => {
      const m = /^\/d\/[^/]+\//.exec(String(globalThis.location?.pathname || ''));
      return m ? m[0].replace(/\/$/, '') : '';
    })();
    const STATUS_URL = API_BASE + '/api/remote-access/status';
    const CONFIG_URL = API_BASE + '/api/remote-access/config';

    /* ── 节点切换：唯一链路（2026-11-10 用户定「只留一套」）──
     * 唯一写入 = relay 的 /__dsh_tunnel__/default-node（POST deviceId）。
     * 唯一跟随 = relay 壳页订阅 default-node/watch(SSE) 切 iframe（0 重载）。
     * 所有入口（会话头部下拉、设置页节点/入口、Agent 工具）都汇到 switchNode()；
     * 不再有插件内 node-watch、整页 reload、或「切完再 POST config 写第二次」。
     * 后端 POST /api/remote-access/config 只作为「无桥环境（桌面直连/旧 relay）」
     * 的兼容代发端点，且由 host 内部复用同一个 default-node 写入，不另起链路。
     *
     * 历史失效模式（已随本收敛删除）：插件外壳注入 iframe 跨站 401、node-watch
     * SSE 断流后整页 reload 兜底装不上、设置页 1.2s 后 location.reload() 与
     * 下拉桥切换双写 default-node —— 都不再存在。 */
    const RELAY_DEFAULT_NODE = '/__dsh_tunnel__/default-node';

    /** 唯一切换写入：POST relay default-node。返回 {ok, error}。
     *  传输回退：桌面直连（127.0.0.1:3082）不经 relay、该路径 404 时，改走
     *  host 的 POST config 代发（host 内部调用同一个 setRelayDefaultNode）。
     *  逻辑写入始终只有一处，不是第二条切换链。 */
    async function switchNode(deviceId) {
      if (!deviceId) return { ok: false, error: '缺少 deviceId' };
      try {
        const res = await fetch(RELAY_DEFAULT_NODE, {
          method: 'POST', headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ deviceId }),
        });
        if (res.ok) return { ok: true };
        if (res.status !== 404) return { ok: false, error: `relay 切换失败 HTTP ${res.status}：${(await res.text()).slice(0, 80)}` };
        // 非 relay 环境：host 代发到同一个 default-node 写入。
        const fb = await fetch(CONFIG_URL, {
          method: 'POST', headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ nodeId: deviceId }),
        });
        const fbBody = await fb.json().catch(() => ({}));
        if (!fb.ok || fbBody.error) return { ok: false, error: fbBody.error || `HTTP ${fb.status}` };
        if (fbBody.relaySync && fbBody.relaySync.ok === false) {
          return { ok: false, error: fbBody.relaySync.error || `relay 同步失败（HTTP ${fbBody.relaySync.status ?? '?'}）` };
        }
        return { ok: true };
      } catch (e) { return { ok: false, error: String(e && e.message || e) }; }
    }
    // 暴露给同页其它组件（设置页）复用同一入口。
    globalThis.__rarSwitchNode = switchNode;

    /* ── 图标（图形化动作按钮用；用户要求：动作一律图形，不用文字按钮）──
     * 统一 24 viewBox / stroke=currentColor / strokeWidth 2，尺寸由调用方给。
     * aria-hidden：图标本身不承载语义，语义在按钮的 aria-label 上。 */
    const ICON_PATHS = {
      refresh: [
        h('path', { key: 'a', d: 'M21 12a9 9 0 1 1-2.64-6.36' }),
        h('polyline', { key: 'b', points: '21 3 21 9 15 9' }),
      ],
      copy: [
        h('rect', { key: 'a', x: 9, y: 9, width: 12, height: 12, rx: 2 }),
        h('path', { key: 'b', d: 'M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1' }),
      ],
      swap: [
        h('polyline', { key: 'a', points: '17 1 21 5 17 9' }),
        h('path', { key: 'b', d: 'M3 11V9a4 4 0 0 1 4-4h14' }),
        h('polyline', { key: 'c', points: '7 23 3 19 7 15' }),
        h('path', { key: 'd', d: 'M21 13v2a4 4 0 0 1-4 4H3' }),
      ],
      check: [h('polyline', { key: 'a', points: '20 6 9 17 4 12' })],
      close: [
        h('line', { key: 'a', x1: 18, y1: 6, x2: 6, y2: 18 }),
        h('line', { key: 'b', x1: 6, y1: 6, x2: 18, y2: 18 }),
      ],
      chevron: [h('polyline', { key: 'a', points: '6 9 12 15 18 9' })],
    };
    function icon(name, size = 16) {
      return h('svg', {
        viewBox: '0 0 24 24',
        width: size,
        height: size,
        fill: 'none',
        stroke: 'currentColor',
        strokeWidth: 2,
        strokeLinecap: 'round',
        strokeLinejoin: 'round',
        'aria-hidden': 'true',
        focusable: 'false',
      }, ICON_PATHS[name]);
    }

    /* ── 样式层（设计稿 03 的落地形态）────────────────────────────────
     * C1：BTN/CARD 内联对象整块删除，换成类名 + 一段注入的 <style>。
     * 颜色走「宿主令牌优先，设计稿值兜底」的双保险：
     *   var(--dsw-alias-<宿主真名>, var(--set-<设计稿值>))
     * 映射表见 design/03-settings-remote-access.html「落地要点」——宿主里
     * --dsw-alias-text-primary 等是幻影（声明数 0），真名是 label-系与 state-系。
     * 几何（radius/行高/密度）不是颜色，不借用宿主令牌，写死在本插件。
     * 注入一次（immediately: true 的模块只 apply 一遍），删除按钮即还原。 */
    const STYLE_ID = 'dsh-remote-access-style';
    const CSS = `
/* ── @local/dsh-remote-access 分区样式（设计稿 03 落地）── */
.rar-section { padding: 4px 0; max-width: 760px; min-width: 0; }
/* 分区标题：13px/600 与行内文字同档（曾 15px + 14px 上边距，段间空白过多）；
   说明（.rar-note）限两行 —— 长解释让位给信息行，读屏与展开后仍可读全文。 */
.rar-secTitle {
  margin: 12px 0 4px;
  font-size: 13px; font-weight: 600; letter-spacing: -0.003em; line-height: 1.3;
  color: var(--dsw-alias-label-primary, var(--set-text, #1d1d1f));
}
.rar-secTitle:first-child { margin-top: 0; }
/* 分区标题行：标题左、动作右（2026-11-08 用户定：刷新按钮挪到「状态」标题右侧）。
   标题与按钮同一条基线，右侧贴齐面板内容边缘。 */
.rar-secHead {
  display: flex; align-items: center; justify-content: space-between;
  gap: 10px; margin: 12px 0 4px; min-width: 0;
}
.rar-secHead:first-child { margin-top: 0; }
.rar-secHead .rar-secTitle { margin: 0; }
/* 视觉右缘对齐下方卡片：按钮 32×32、图标 16。不挪按钮盒（会溢出），
   只把图标在按钮内右贴（justify-items:end），让图标右缘贴齐卡片右缘。 */
.rar-secHead .rar-iconbtn { justify-items: end; }
.rar-secTitle + .rar-note, .rar-note { margin: 0 0 6px; }
.rar-note {
  display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical;
  overflow: hidden;
}
.rar-note, .rar-help {
  font-size: 11px; line-height: 1.5; letter-spacing: .01em;
  color: var(--dsw-alias-label-secondary, var(--set-text-2, #6e6e73));
  min-width: 0; overflow-wrap: anywhere;
}
/* inset-grouped 行容器：fill 用宿主设置卡档（bg-layer-2），不叠第二层投影 */
.rar-group {
  background: var(--dsw-alias-settings-card-fill, var(--dsw-alias-bg-layer-2, var(--set-surface, #fff)));
  border-radius: 12px;
  box-shadow: inset 0 0 0 1px var(--dsw-alias-border-l1, var(--set-line, rgba(0,0,0,.08)));
  overflow: hidden; min-width: 0;
}
.rar-group--pad { padding: 12px 14px; }
.rar-row {
  display: flex; align-items: center; justify-content: space-between;
  gap: 10px; min-height: 32px; padding: 5px 14px; min-width: 0;
  font-size: 13px; line-height: 1.45;
}
.rar-row + .rar-row { box-shadow: inset 0 1px 0 var(--dsw-alias-border-l1, var(--set-line, rgba(0,0,0,.08))); }
/* iOS 惯例：label 定宽不缩（「结论」在 390px 被挤成"结/论"两行就是这么来的），
 * value 占剩余宽度、右对齐、可换行 —— 长值（verdict/unit 名）折行而不是挤压 label。 */
.rar-row__label { flex: 0 0 auto; min-width: 0; color: var(--dsw-alias-label-primary, var(--set-text, #1d1d1f)); }
.rar-row__value {
  flex: 1 1 auto; min-width: 0; text-align: right; overflow-wrap: anywhere;
  color: var(--dsw-alias-label-secondary, var(--set-text-2, #6e6e73));
  font-variant-numeric: tabular-nums;
}
.rar-row__value--mono { font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace; font-size: 12px; }
/* 胶囊控件（2026-11-08 用户给定 .wb-badge 控件规范，.rar-pill 按其骨架重写）。
   四档语义：good/warn/bad/中性；色点 currentColor；ink 承载语义、颜色只是冗余编码
   （每枚都有文字，不靠颜色猜）。染底 12% / 描边 30% / 3px 10px / 12px 500 /
   --set-radius-sm 圆角 / 组内 flex wrap 8px —— 与用户控件逐值一致。
   color-mix 与宿主 Tag 同式；这是插件注入样式，跑在宿主浏览器，不受设计稿
   chrome100 闸门约束。 */
.rar-pill {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 3px 10px; border-radius: var(--dsw-alias-radius-control, var(--set-radius-sm, 8px));
  font-size: 12px; line-height: 1.6; font-weight: 500;
  letter-spacing: .01em; white-space: nowrap; min-width: 0;
  background: var(--dsw-alias-bg-module-platform, var(--dsw-alias-bg-layer-2, var(--set-surface-2, #fafafc)));
  color: var(--dsw-alias-label-secondary, var(--set-text-2, #6e6e73));
  border: 1px solid var(--dsw-alias-border-l1, var(--set-line, rgba(0,0,0,.08)));
}
.rar-pill__dot { flex: 0 0 auto; width: 6px; height: 6px; border-radius: 999px; background: currentColor; }
.rar-pill--good {
  color: var(--dsw-alias-state-success-primary, var(--set-ok-ink, #178143));
  background: color-mix(in srgb, var(--dsw-alias-state-success-primary, #1a8f4a) 12%, var(--dsw-alias-bg-layer-2, var(--set-surface, #ffffff)));
  border-color: color-mix(in srgb, var(--dsw-alias-state-success-primary, #1a8f4a) 30%, transparent);
}
.rar-pill--warn {
  color: var(--dsw-alias-state-warn-label, var(--set-warn-ink, #b25000));
  background: color-mix(in srgb, var(--dsw-alias-state-warn-primary, #b25000) 12%, var(--dsw-alias-bg-layer-2, var(--set-surface, #ffffff)));
  border-color: color-mix(in srgb, var(--dsw-alias-state-warn-primary, #b25000) 30%, transparent);
}
.rar-pill--bad {
  color: var(--dsw-alias-state-error-primary, var(--set-danger-ink, #d70015));
  background: color-mix(in srgb, var(--dsw-alias-state-error-primary, #d70015) 12%, var(--dsw-alias-bg-layer-2, var(--set-surface, #ffffff)));
  border-color: color-mix(in srgb, var(--dsw-alias-state-error-primary, #d70015) 30%, transparent);
}
/* 值列里的胶囊组：右对齐、可换行（服务单元可能 2–3 个）；间距取控件规范的 8px */
.rar-row__value--pills { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; justify-content: flex-end; }
.rar-row__value-text { min-width: 0; overflow-wrap: anywhere; }
/* 列表条目（inset-grouped 行，取代旧版浮起卡）：一行一个节点/入口。
   head = 名称 + 灰色 id/URL；body = details 展开后的紧凑行。
   选中态（设计稿 03 .ep--on 的实装对应物，2026-11-11 对齐）：2px inset 绿框 ——
   分组表面已有自身描边，卡上叠第二层投影就是「两层浮起」；Apple 的层级靠 fill，
   选中语义走框色 + 行尾徽章（颜色只是冗余编码，词承载语义）。 */
.rar-item { min-width: 0; border-radius: 10px; }
.rar-item + .rar-item { box-shadow: inset 0 1px 0 var(--dsw-alias-border-l1, var(--set-line, rgba(0,0,0,.08))); }
/* 选中条目的行缝会压在 2px 框上（上一条的 inset 0 1px 落在本条顶部），
   选中态整行取回自己的完整框，不吃行缝。 */
.rar-item.rar-item--on { box-shadow: inset 0 0 0 2px var(--dsw-alias-state-success-primary, var(--set-ok, #1a8f4a)); }
.rar-item.rar-item--on + .rar-item { box-shadow: none; }
.rar-item__head {
  display: flex; align-items: center; justify-content: space-between; gap: 10px;
  min-height: 32px; padding: 6px 14px;
  cursor: pointer; list-style: none; min-width: 0;
}
.rar-item__head::-webkit-details-marker { display: none; }
.rar-item__main { flex: 1 1 auto; min-width: 0; display: flex; align-items: baseline; gap: 8px; }
.rar-item__name { min-width: 0; font-size: 13px; font-weight: 600; overflow-wrap: anywhere; color: var(--dsw-alias-label-primary, var(--set-text, #1d1d1f)); }
.rar-item__id {
  min-width: 0; font-size: 11px; overflow-wrap: anywhere;
  color: var(--dsw-alias-label-tertiary, var(--set-text-2, #6e6e73));
}
.rar-item__id--mono { font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace; }
.rar-item__badge {
  flex: 0 0 auto; display: inline-flex; align-items: center; gap: 6px;
  padding: 3px 10px; border-radius: var(--dsw-alias-radius-control, var(--set-radius-sm, 8px));
  font-size: 12px; line-height: 1.6; font-weight: 500; white-space: nowrap;
  color: var(--dsw-alias-state-success-primary, var(--set-ok-ink, #178143));
  background: color-mix(in srgb, var(--dsw-alias-state-success-primary, #1a8f4a) 12%, var(--dsw-alias-bg-layer-2, var(--set-surface, #ffffff)));
  border: 1px solid color-mix(in srgb, var(--dsw-alias-state-success-primary, #1a8f4a) 30%, transparent);
}
.rar-item__body { padding: 0 14px 8px; }
/* 图形化动作按钮（用户要求：动作一律图形，不用文字按钮）。
 * 视觉 32×32（图标 16），命中区由 ::before 扩到 44×44；品牌色。
 * busy 态图标旋转 = 正在工作（只碰 transform，动效白名单内），
 * 语义不靠颜色：aria-label 同时切到「正在检查…/切换中…」。 */
.rar-iconbtn {
  flex: 0 0 auto;
  position: relative;
  display: inline-grid; place-items: center;
  width: 32px; height: 32px; padding: 0; margin: 0;
  color: var(--dsw-alias-link, var(--set-brand-ink, #0066cc));
  background: transparent; border: 0; border-radius: 8px;
  cursor: pointer; touch-action: manipulation;
  /* 动效白名单（闸门 G10）：只碰 transform/opacity。hover 底色交给宿主
     interactive-bg-hover（有 hover 设备才出现），不进 transition。 */
  transition: transform 120ms cubic-bezier(0.25, 0.46, 0.45, 0.94),
              opacity 120ms cubic-bezier(0.25, 0.46, 0.45, 0.94);
}
.rar-iconbtn::before { content: ""; position: absolute; inset: -6px; }
.rar-iconbtn svg { display: block; }
.rar-iconbtn:active { transform: scale(.97); }
.rar-iconbtn[disabled] { opacity: .45; cursor: default; transform: none; }
.rar-iconbtn:focus-visible { outline: 2px solid var(--dsw-alias-focus-ring, var(--set-brand, #0071e3)); outline-offset: 1px; }
.rar-iconbtn--busy svg { animation: rar-spin 1s linear infinite; }
@keyframes rar-spin { to { transform: rotate(360deg); } }
@media (prefers-reduced-motion: reduce) {
  .rar-iconbtn--busy svg { animation-duration: 2s; }   /* 减弱动态：放慢但不停（进度必须可观测） */
}
/* 按压反馈（C8）：pointerdown 挂类，:active 兜底；只加类不 preventDefault。
   文字按钮已按用户要求全部换成图形按钮（.rar-iconbtn），旧的 .rar-btn 样式删除。 */
.rar-iconbtn.is-pressed { transform: scale(.97); }
/* 密度：同类项折叠（设计稿 03 同款）。summary 双档高（触屏 44 / 桌面 32），
   chevron 用 transform 旋转（只碰 transform，动效白名单内）。 */
.rar-disclose { min-width: 0; }
.rar-disclose__s {
  display: flex; align-items: center; gap: 12px;
  min-height: 32px; padding: 0 14px;
  cursor: pointer; list-style: none;
  font-size: 13px; color: var(--dsw-alias-label-secondary, var(--set-text-2, #6e6e73));
  min-width: 0;
}
.rar-disclose__s::-webkit-details-marker { display: none; }
.rar-disclose__s::after {
  content: '\\203A'; margin-left: auto; flex: 0 0 auto;
  font-size: 15px;
  transform: rotate(90deg);
  transition: transform 160ms cubic-bezier(0.25, 0.46, 0.45, 0.94);
}
.rar-disclose[open] > .rar-disclose__s::after { transform: rotate(-90deg); }
.rar-disclose__body { padding: 2px 14px 6px 40px; }   /* 左缩进对齐上一行的值列 */
.rar-row--compact { min-height: 26px; padding: 3px 0; font-size: 12px; }
.rar-row--compact + .rar-row--compact { box-shadow: none; }
/* ── C1 内联清零（2026-11-11 对齐设计稿 03）：以下类承接此前写死在 JSX
   style 对象里的布局（marginTop 区块间距、动作行右对齐、QR 容器、输入框）。
   React 里 style 对象每次 render 都新建；类只注入一次，主题切换也只走令牌。
   两处运行时定位（nmenu 的 fixed 坐标、nswitch 无标签行时的 fixed 兜底）
   保留内联 —— 值来自 getBoundingClientRect，不是设计常量。 ── */
.rar-block { margin-top: 14px; }
.rar-acts-r { display: flex; justify-content: flex-end; gap: 6px; }
.rar-acts-r--center { align-items: center; }
.rar-qr { display: flex; flex-direction: column; align-items: center; gap: 8px; padding: 10px 0; min-width: 0; }
/* QR 容器（稿面 .pair__qr 的实装对应物）：码有 164px 视觉框 + 静区，
   过期变暗（不透明度降档），倒计时是事实不是装饰。 */
.rar-qr__box {
  width: 164px; height: 164px; display: grid; place-items: center;
  border-radius: 8px; background: var(--dsw-alias-bg-base, var(--set-surface, #fff));
  box-shadow: inset 0 0 0 1px var(--dsw-alias-border-l1, var(--set-line, rgba(0,0,0,.08)));
  overflow: hidden;
}
.rar-qr__box svg { display: block; max-width: 100%; max-height: 100%; }
.rar-qr__box--expired { opacity: .45; }
.rar-facts { margin-top: 10px; }
.rar-input {
  width: 180px; font: inherit; font-size: 12px; padding: 4px 8px;
  border-radius: 8px; border: 0;
  box-shadow: inset 0 0 0 1px var(--dsw-alias-border-l1, rgba(0,0,0,.08));
  background: transparent; color: inherit; min-width: 0;
}
@media (max-width: 767px) {
  .rar-input { width: 132px; }
}
.rar-empty {
  padding: 20px 14px; text-align: center; min-width: 0;
  color: var(--dsw-alias-label-secondary, var(--set-text-2, #6e6e73));
}
.rar-empty__title { font-size: 14px; margin: 0 0 4px; }
.rar-empty__detail { font-size: 12px; margin: 0; overflow-wrap: anywhere; color: var(--dsw-alias-label-tertiary, var(--set-text-2, #6e6e73)); }
.rar-banner {
  display: flex; align-items: flex-start; gap: 8px;
  padding: 9px 12px; border-radius: 12px; font-size: 13px; line-height: 1.45; min-width: 0;
  overflow-wrap: anywhere;
}
.rar-banner--bad {
  color: var(--dsw-alias-state-error-primary, var(--set-danger-ink, #d70015));
  background: color-mix(in srgb, var(--dsw-alias-state-error-primary, #d70015) 8%, transparent);
}
.rar-banner--warn {
  color: var(--dsw-alias-state-warn-label, var(--set-warn-ink, #b25000));
  background: color-mix(in srgb, var(--dsw-alias-state-warn-label, #b25000) 8%, transparent);
}
/* 成功回执（tiMsg/pairMsg tone==='good' 的发射点）：此前 JSX 引用 --ok 而 CSS
   无声明、颜色靠内联 style 双源补 —— 归一到类（C1 内联清零的一部分）。 */
.rar-banner--ok {
  color: var(--dsw-alias-state-success-primary, var(--set-ok-ink, #178143));
  background: color-mix(in srgb, var(--dsw-alias-state-success-primary, #1a8f4a) 8%, transparent);
}
.rar-banner code { font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 12px; }
/* 状态切换入场（C9）：.rar-stagger 挂在容器上，子元素逐条 50ms 落位；
 * 刷新完成后的重绘不挂（数据更新不是新内容出现）。 */
.rar-stagger > * {
  animation: rar-enter 400ms cubic-bezier(0.25, 0.46, 0.45, 0.94) both;
  animation-delay: calc(50ms * var(--rar-i, 0));
}
.rar-stagger > *:nth-child(1) { --rar-i: 0; }
.rar-stagger > *:nth-child(2) { --rar-i: 1; }
.rar-stagger > *:nth-child(3) { --rar-i: 2; }
.rar-stagger > *:nth-child(4) { --rar-i: 3; }
.rar-stagger > *:nth-child(5) { --rar-i: 4; }
.rar-stagger > *:nth-child(6) { --rar-i: 5; }
.rar-stagger > *:nth-child(n + 7) { --rar-i: 5; }
@keyframes rar-enter {
  from { opacity: 0; transform: translateY(4px) scale(.96); }
  to   { opacity: 1; transform: none; }
}
@media (prefers-reduced-motion: reduce) {
  .rar-stagger > * { animation-duration: .01ms; }
  .rar-iconbtn { transition-duration: .01ms; }
  .rar-iconbtn.is-pressed, .rar-iconbtn:active { transform: none; }
}
/* ── 会话头部节点下拉开关（2026-11-09 用户定：红框=标签行右端）──
 * 触发器 = 当前节点 deviceId + chevron；点开 = 节点菜单。
 * 菜单样式 2026-11-10 用户给定（红圈参考图）：白卡大圆角，一行 =
 * 状态色点 + deviceId 大字 + 行尾选中勾；无标题/副行/胶囊。
 * 2026-11-10 二轮微调（用户）：触发器去背景色、整体上移 2px、菜单色点缩小。
 * 菜单 portal 到 body（头部 overflow:clip，行内菜单会被裁掉）。 */
.rar-nswitch { position: relative; display: inline-flex; flex: none; min-width: 0; }
.rar-nswitch__btn {
  display: inline-flex; align-items: center; gap: 6px;
  max-width: 180px; min-height: 28px; padding: 3px 10px; margin: 0;
  /* portal 进标签行（flex）时 auto 左边距把按钮推到行尾 = 用户红框处；
     回退在 utilities 的 inline-flex 占位 span 里时 auto=0，无副作用。 */
  margin-left: auto; align-self: center;
  /* 2026-11-10 用户：触发器不要背景色（无框无底的纯文本+chevron，
     点击命中区照旧）。聚焦环走 :focus-visible，hover 无底色。 */
  border: 0;
  border-radius: var(--dsw-alias-radius-control, var(--set-radius-sm, 8px));
  background: transparent;
  color: var(--dsw-alias-label-primary, var(--set-text, #1d1d1f));
  font: inherit; font-size: 13px; line-height: 1.5; font-weight: 400;
  cursor: pointer; touch-action: manipulation; min-width: 0;
}
/* 2026-11-10 用户：按钮整体往上挪一点（标签行内不再垂直居中）。 */
.rar-nswitch__btn { transform: translateY(-2px); }
.rar-nswitch__btn::before { content: ""; position: absolute; inset: -6px 0; }
.rar-nswitch__btn { position: relative; }
/* 标签行是 flex-wrap:wrap：窄屏放不下时按钮独占一行也贴右端，不挤标签。 */
.rar-nswitch__btn { flex: 0 1 auto; }
.rar-nswitch__label { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.rar-nswitch__btn svg { flex: 0 0 auto; transition: transform 160ms cubic-bezier(0.25, 0.46, 0.45, 0.94); }
.rar-nswitch__btn[aria-expanded="true"] svg { transform: rotate(180deg); }
.rar-nswitch__btn:focus-visible { outline: 2px solid var(--dsw-alias-focus-ring, var(--set-brand, #0071e3)); outline-offset: 1px; }
.rar-nswitch__btn[disabled] { opacity: .55; cursor: default; }
.rar-nmenu {
  position: fixed; z-index: 1200; box-sizing: border-box;
  /* 宽度随内容收缩（用户：卡片太宽）：fixed 无 left/right 约束时 auto 宽
     = 收缩到最长行；min 兜两行短 id 不挤，max 兜长 id 不超视口。 */
  width: max-content; min-width: 180px; max-width: min(320px, calc(100vw - 16px));
  max-height: min(60vh, 420px); overflow-y: auto; overscroll-behavior: contain;
  padding: 10px 8px; border-radius: 16px;
  background: var(--dsw-alias-settings-card-fill, var(--dsw-alias-bg-layer-2, var(--set-surface, #fff)));
  box-shadow: 0 10px 32px rgba(0,0,0,.14), inset 0 0 0 1px var(--dsw-alias-border-l1, var(--set-line, rgba(0,0,0,.06)));
}
/* 红圈样式：一行 = 色点 + deviceId + 行尾勾；行高与字号对齐会话标签行
 * （用户 2026-11-10：下拉字号太大）。触屏 44px 命中区靠 padding 保住。 */
.rar-nmenu__item {
  display: flex; align-items: center; gap: 10px; width: 100%;
  min-height: 34px; padding: 6px 12px; margin: 0; border: 0; border-radius: 8px;
  background: transparent; color: var(--dsw-alias-label-primary, var(--set-text, #1d1d1f));
  font: inherit; font-size: 13px; line-height: 1.4; text-align: left;
  cursor: pointer; touch-action: manipulation; min-width: 0;
}
.rar-nmenu__item[disabled] { cursor: default; }
@media (hover: hover) {
  .rar-nmenu__item:not([disabled]):hover { background: var(--dsw-alias-interactive-bg-hover, rgba(0,0,0,.05)); }
}
.rar-nmenu__item:focus-visible { outline: 2px solid var(--dsw-alias-focus-ring, var(--set-brand, #0071e3)); outline-offset: -2px; }
/* 2026-11-10 用户：色点缩小（10→7px）。 */
.rar-nmenu__dot { flex: 0 0 auto; width: 7px; height: 7px; border-radius: 999px; background: #98989d; }
.rar-nmenu__dot--on { background: var(--dsw-alias-state-success-primary, var(--set-ok-ink, #1a8f4a)); }
.rar-nmenu__dot--off { background: var(--dsw-alias-state-error-primary, var(--set-danger-ink, #d70015)); }
.rar-nmenu__name { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 400; }
.rar-nmenu__check { flex: 0 0 auto; color: var(--dsw-alias-state-success-primary, var(--set-ok-ink, #178143)); display: inline-flex; }
.rar-nmenu__err { margin: 4px 4px 2px; padding: 7px 10px; border-radius: 8px; font-size: 12px; line-height: 1.45; overflow-wrap: anywhere;
  color: var(--dsw-alias-state-error-primary, var(--set-danger-ink, #d70015));
  background: color-mix(in srgb, var(--dsw-alias-state-error-primary, #d70015) 8%, transparent); }
.rar-nmenu__busy { flex: 0 0 auto; display: inline-flex; color: var(--dsw-alias-link, var(--set-brand-ink, #0066cc)); }
.rar-nmenu__busy svg { animation: rar-spin 1s linear infinite; }
@media (max-width: 767px) {
  .rar-nswitch__btn { max-width: 150px; min-height: 30px; font-size: 13px; }
  .rar-nmenu__item { min-height: 34px; font-size: 13px; }
}
@media (prefers-reduced-motion: reduce) {
  .rar-nswitch__btn svg { transition-duration: .01ms; }
  .rar-nmenu__busy svg { animation-duration: 2s; }
}
/* 节点开关浮层兜底 z-index（relay 壳页场景下开关在节点 GUI 头部里） */
[data-rar-nswitch] { position: relative; z-index: 30; }
/* C10 移动档（≤767px，同宿主 SettingsRoot 断点）：
 * 390px 实测桌面内联样式在手机上是坏的（长 URL 挤压切换按钮、12px 小字）。
 * 密度回触屏档：行 44px、15px 字号；横滚写在滚动元素自己身上、父级
 * min-width:0（漏一条整页溢出 345px，两处各踩过一次）。 */
@media (max-width: 767px) {
  .rar-section { max-width: none; }
  .rar-row { min-height: 44px; font-size: 15px; padding: 8px 14px; }
  .rar-disclose__s { min-height: 44px; font-size: 15px; padding: 0 14px; }
  .rar-row--compact { min-height: 40px; font-size: 13px; }
  .rar-disclose__body { padding: 2px 14px 6px 14px; }
  .rar-group--pad { padding: 10px 12px; }
  .rar-item__head { min-height: 44px; padding: 8px 14px; }
  .rar-item__name { font-size: 15px; }
  .rar-pill, .rar-item__badge { font-size: 12px; padding: 3px 10px; }
  .rar-iconbtn { width: 44px; height: 44px; }
  .rar-secHead .rar-iconbtn { justify-items: end; }
  .rar-secTitle { font-size: 16px; }
  .rar-banner { font-size: 14px; }
  .rar-empty { padding: 24px 14px; }
}
`;
    function injectStyleOnce() {
      if (document.getElementById(STYLE_ID)) return;
      const el = document.createElement('style');
      el.id = STYLE_ID;
      el.textContent = CSS;
      document.head.appendChild(el);
    }

    /* 按压反馈（C8）：与 App 同式 —— pointerdown 立刻给反馈，不等抬起。
     * 只加类、绝不 preventDefault（不抢手势、不阻断输入）。 */
    function installPressFeedback() {
      if (window.__rarPressInstalled) return;
      window.__rarPressInstalled = true;
      document.addEventListener('pointerdown', (event) => {
        const el = event.target.closest?.('.rar-iconbtn, .rar-item__head');
        if (!el || el.disabled) return;
        el.classList.add('is-pressed');
        const off = () => {
          el.classList.remove('is-pressed');
          document.removeEventListener('pointerup', off);
          document.removeEventListener('pointercancel', off);
        };
        document.addEventListener('pointerup', off);
        document.addEventListener('pointercancel', off);
      });
      document.addEventListener('visibilitychange', () => {
        if (document.visibilityState === 'hidden') {
          document.querySelectorAll('.is-pressed').forEach(el => el.classList.remove('is-pressed'));
        }
      });
    }


    /** online is true / false / 'unknown' — never guess. */
    function onlineTone(online) {
      return online === true ? 'good' : online === false ? 'bad' : undefined;
    }
    function onlineText(t, online) {
      return online === true ? t('node.online') : online === false ? t('node.offline') : t('node.unknown');
    }

    /** Service states are platform-specific: systemd 'active' or Windows 'running'. */
    function serviceTone(state) {
      // C6②：start_pending / stop_pending 是过渡态不是失败 → warn。
      if (state === 'active' || state === 'running') return 'good';
      if (state === 'start_pending' || state === 'stop_pending') return 'warn';
      if (state === 'unknown') return undefined;
      return 'bad';
    }
    /** Relay 层（C6①）：reachable 但 deviceOnline=false = 链路断 → warn，不是 bad。 */
    function relayTone(reachable, deviceOnline) {
      if (reachable !== true) return 'bad';
      return deviceOnline === true ? 'good' : 'warn';
    }

    /** 状态胶囊发射点（2026-11-08 控件规范 .wb-badge）：每枚都带 currentColor
     *  色点；tone 缺省 = 中性灰。颜色只是冗余编码，词承载语义。 */
    const pill = (tone, text) =>
      h('span', { className: `rar-pill${tone ? ` rar-pill--${tone}` : ''}` },
        h('span', { className: 'rar-pill__dot' }), text);

    /** 空列表（C6③）：中性空状态 + 点名该改哪个文件；danger 只留给请求失败。 */
    function EmptyState(t, title, detail) {
      return h('div', { className: 'rar-empty' },
        h('p', { className: 'rar-empty__title' }, title),
        h('p', { className: 'rar-empty__detail' }, detail));
    }

    /** Status block（inset-grouped 行容器，C2/C3 + 密度：同类项合并）。
     *  状态值统一用只读胶囊（.rar-pill）：颜色只是冗余编码，词承载语义。
     *  服务单元合并成一行胶囊摘要，逐条明细进原生 <details>（默认收起、键盘可达）——
     *  原文一字不改，只是不默认展开。状态区从 (2+服务数+1) 行降到 4 行。 */
    function StatusBlock(props) {
      const { t, status, error, animate, cfgLoaded } = props;
      const rows = [];
      if (error !== null) {
        rows.push(h('div', { key: 'err', className: 'rar-banner rar-banner--bad' },
          t('status.failPrefix'), h('code', null, error), t('status.failSuffix')));
      } else if (status !== null) {
        const s = status.snapshot;
        const relayT = relayTone(s.relay.reachable, s.relay.deviceOnline);
        rows.push(h('div', { key: 'ep', className: 'rar-row' },
          h('span', { className: 'rar-row__label' }, t('status.endpoint')),
          h('span', { className: 'rar-row__value' }, status.endpointLabel || status.endpointId || '—')));
        const nodeTone = status.node ? (onlineTone(status.node.online) || 'neutral') : 'neutral';
        rows.push(h('div', { key: 'node', className: 'rar-row' },
          h('span', { className: 'rar-row__label' }, t('status.node')),
          h('span', { className: 'rar-row__value rar-row__value--pills' },
            status.node
              ? [
                h('span', { key: 'txt', className: 'rar-row__value-text' }, `${status.node.label} (${status.node.deviceId})`),
                h('span', { key: 'pill' }, pill(nodeTone, onlineText(t, status.node.online))),
              ]
              : t('node.none'))));
        rows.push(h('div', { key: 'relay', className: 'rar-row' },
          h('span', { className: 'rar-row__label' }, t('status.relay')),
          h('span', { className: 'rar-row__value rar-row__value--pills' },
            pill(relayT,
              s.relay.reachable === true
                ? (s.relay.deviceOnline === true ? t('status.deviceOnline') : t('status.deviceOffline'))
                : t('status.unreachable')))));
        // 密度：服务单元合并成一行胶囊摘要；全部一致时不加注，不一致时补一枚
        // warn 胶囊「状态不一致」——摘要必须能承担"不展开也知道有事"的职责。
        const services = s.services || [];
        if (services.length > 0) {
          const states = services.map((svc) => String(svc.state));
          const allSame = states.every((x) => x === states[0]);
          rows.push(h('div', { key: 'svc-summary', className: 'rar-row' },
            h('span', { className: 'rar-row__label' }, t('status.services')),
            h('span', { className: 'rar-row__value rar-row__value--pills' },
              services.map((svc) => pill(serviceTone(svc.state), String(svc.state))),
              allSame ? null : pill('warn', t('status.mismatch')))));
          // 明细收进 details：unit 名原文（svc.label || svc.name）+ 状态胶囊。
          rows.push(h('details', { key: 'svc-detail', className: 'rar-disclose' },
            h('summary', { className: 'rar-disclose__s' },
              `${t('status.servicesDetail')}（${services.length}）`),
            h('div', { className: 'rar-disclose__body' },
              services.map((svc) => {
                const tone = serviceTone(svc.state);
                return h('div', { key: `svc-${svc.id}`, className: 'rar-row rar-row--compact' },
                  h('span', { className: 'rar-row__label' }, svc.label || svc.name),
                  h('span', { className: 'rar-row__value rar-row__value--pills' },
                    pill(tone, String(svc.state))));
              }))));
        }
        rows.push(h('div', { key: 'verdict', className: 'rar-row' },
          h('span', { className: 'rar-row__label' }, t('status.verdict')),
          h('span', { className: 'rar-row__value rar-row__value--mono' }, status.verdict)));
      } else {
        // 两种"未检查"不是同一件事，文案必须区分（C7，稿面 F2/F5 的分支）：
        // 配置也没回 = 两个请求都还没回；配置已读到 = 只有状态没查。
        // 区分依据由 RemoteAccessSection 经 props 传入（它同时持有 cfg 态）。
        rows.push(h('p', { key: 'idle', className: 'rar-banner rar-banner--warn' },
          t('status.idle'), ' —— ', cfgLoaded ? t('status.idleOnlyStatus') : t('status.idleDetail')));
      }
      return h('div', { className: animate ? 'rar-stagger' : undefined }, rows);
    }

    /** Controlled-node list —— 密度重做（2026-11-07 二轮）：去卡片化。
     *  旧版每节点一块浮起卡（padding 12 + gap 8），三行只说一件事
     *  （名称/deviceId/URL 是同一台机器），四周空白占 30%。
     *  新版 = inset-grouped 行，与状态区同一种容器：
     *    · 一行 = 名称 + deviceId 灰字并列；URL 收进行内 details
     *    · 选中态 = 行尾绿字「使用中」（色+词），非选中 = 行尾「切换」文字动作
     *    · 复制地址 = URL 行内的小动作，不再独占一行
     *    · 私有 CA 有才显示（缺省不占位） */
    function NodeList(props) {
      const { t, cfg, onSwitch, switching, error } = props;
      if (cfg === null) return h('p', { className: 'rar-help' }, t('config.loading'));
      const list = cfg.nodes || [];
      if (list.length === 0) {
        // C6③：空列表不是"配错了"，中性空状态 + 数量 + 点名配置处。
        return EmptyState(t, `${t('node.none')}（0 个）`, t('node.empty'));
      }
      const items = list.map((node) => {
        const body = [
          // URL 行 + 行尾复制图标：复制从"独占一行"降为行内动作（图形按钮）
          h('div', { key: 'u', className: 'rar-row rar-row--compact' },
            h('span', { className: 'rar-row__label' }, t('node.urlLabel')),
            h('span', { className: 'rar-row__value rar-row__value--mono' }, node.url),
            h('button', {
              key: 'cp',
              onClick: () => { try { void navigator.clipboard.writeText(node.url); } catch { /* clipboard may be unavailable */ } },
              className: 'rar-iconbtn',
              'aria-label': t('node.copy'),
              title: t('node.copy'),
            }, icon('copy', 15))),
        ];
        if (node.caFile) {
          body.push(h('div', { key: 'ca', className: 'rar-row rar-row--compact' },
            h('span', { className: 'rar-row__label' }, t('config.caFile')),
            h('span', { className: 'rar-row__value rar-row__value--mono' }, node.caFile)));
        }
        const switchLabel = switching ? t('node.switching') : t('node.switch');
        return h('details', { key: node.id, className: `rar-item${node.active ? ' rar-item--on' : ''}` },
          h('summary', { className: 'rar-item__head' },
            h('span', { className: 'rar-item__main' },
              h('span', { className: 'rar-item__name' }, node.label),
              h('span', { className: 'rar-item__id' }, node.deviceId)),
            node.active
              ? h('span', { className: 'rar-item__badge' }, icon('check', 13), t('config.active'))
              : h('button', {
                onClick: (ev) => { ev.preventDefault(); onSwitch({ nodeId: node.id }); },
                disabled: switching,
                className: `rar-iconbtn${switching ? ' rar-iconbtn--busy' : ''}`,
                'aria-label': switchLabel,
                title: switchLabel,
              }, icon('swap'))),
          h('div', { className: 'rar-item__body' }, body));
      });
      return h('div', { className: 'rar-group' },
        items,
        error !== null ? h('div', { className: 'rar-banner rar-banner--bad' }, error) : null);
    }

    /** Relay entrance list —— 同上，去卡片化（一行一入口）。 */
    function EndpointList(props) {
      const { t, cfg, onSwitch, switching } = props;
      if (cfg === null) return h('p', { className: 'rar-help' }, t('config.loading'));
      const list = cfg.endpoints || [];
      if (list.length === 0) {
        return EmptyState(t, `${t('config.none')}（0 个）`, t('config.empty'));
      }
      const items = list.map((ep) => {
        const body = [
          h('div', { key: 'u', className: 'rar-row rar-row--compact' },
            h('span', { className: 'rar-row__label' }, t('node.urlLabel')),
            h('span', { className: 'rar-row__value rar-row__value--mono' }, ep.probeUrl)),
        ];
        if (ep.caFile) {
          body.push(h('div', { key: 'ca', className: 'rar-row rar-row--compact' },
            h('span', { className: 'rar-row__label' }, t('config.caFile')),
            h('span', { className: 'rar-row__value rar-row__value--mono' }, ep.caFile)));
        }
        const switchLabel = switching ? t('config.switching') : t('config.switch');
        return h('details', { key: ep.id, className: `rar-item${ep.active ? ' rar-item--on' : ''}` },
          h('summary', { className: 'rar-item__head' },
            h('span', { className: 'rar-item__main' },
              h('span', { className: 'rar-item__name' }, ep.label),
              h('span', { className: 'rar-item__id rar-item__id--mono' }, ep.probeUrl)),
            ep.active
              ? h('span', { className: 'rar-item__badge' }, icon('check', 13), t('config.active'))
              : h('button', {
                onClick: (ev) => { ev.preventDefault(); onSwitch({ endpointId: ep.id }); },
                disabled: switching,
                className: `rar-iconbtn${switching ? ' rar-iconbtn--busy' : ''}`,
                'aria-label': switchLabel,
                title: switchLabel,
              }, icon('swap'))),
          h('div', { className: 'rar-item__body' }, body));
      });
      return h('div', { className: 'rar-group' }, items);
    }

    /** 会话头部节点下拉开关（2026-11-09 用户需求：红框位置=标签行右端）。
     *  座位 = shell.overlay（常驻）；渲染时 portal 进当前会话头部的标签行
     *  （[data-conversation-tabs] 的父容器，margin-left:auto 贴右端 = 红框处）。
     *  没有标签行（空白会话/单视图）时固定右上角兜底。
     *  切换逻辑只留一套（2026-11-10 用户定）：插件下拉、设置页、Agent 工具
     *  全部汇到 switchNode() 直发 relay default-node；壳页仅 SSE 跟随切 iframe。
     *  本页跑在壳页的同源 iframe 里时，经 window.parent.__dshShell 桥只做
     *  「读」：state() 读在线态/当前节点、subscribe() 听切换通知；桥的
     *  switchTo/takeover/release/beat 均为兼容空实现，不承载写入。
     *  无桥（桌面直连/旧 relay）时 switchNode 走 host config POST 代发（同一处
     *  服务端写入），无整页 reload。
     *  在线态诚实性：devices 列表缺失（老 relay/未授权）→ 不显示在线点，不猜。 */
    function shellBridge() {
      try {
        if (globalThis.window === globalThis.window.top) return null;
        const b = globalThis.window.parent.__dshShell;
        // 桥只用于「读」：state/subscribe 必须在。switchTo 已是兼容空实现，
        // 不再作为可用判据（收敛后写入不走桥）。
        return b && typeof b.state === 'function' && typeof b.subscribe === 'function' ? b : null;
      } catch { return null; } // 跨域 parent（不该发生）：回退无桥路径
    }
    function NodeSwitch(props) {
      const t = props.t;
      const [cfg, setCfg] = React.useState(null);
      const [online, setOnline] = React.useState(null);   // Map deviceId→bool | null（未观测）
      const [open, setOpen] = React.useState(false);
      const [switching, setSwitching] = React.useState(null); // 正在切换的 nodeId | null
      const [error, setError] = React.useState(null);
      const [seat, setSeat] = React.useState(undefined);  // undefined=未找, null=无标签行, Element=标签行
      const [pos, setPos] = React.useState(null);         // 菜单 fixed 坐标
      const [visible, setVisible] = React.useState(null); // 桥接管的当前可见 deviceId
      const btnRef = React.useRef(null);
      const menuRef = React.useRef(null);
      const rootRef = React.useRef(null);

      // 桥订阅：读权威状态、听切换通知。takeover/beat/release 是壳页兼容空
      // 实现（壳页已无顶栏），保留调用以兼容旧版壳页；不依赖它们的副作用。
      React.useEffect(() => {
        const bridge = shellBridge();
        if (!bridge) return;
        bridge.takeover?.();
        bridge.beat?.();
        const st = bridge.state();
        setVisible(st.current);
        if (Array.isArray(st.devices)) setOnline(new Map(st.devices.map((d) => [d.id, d.online === true])));
        const off = bridge.subscribe((deviceId) => {
          setVisible(deviceId);
          const s = bridge.state();
          if (Array.isArray(s.devices)) setOnline(new Map(s.devices.map((d) => [d.id, d.online === true])));
        });
        return () => { off(); bridge.release?.(); };
      }, []);

      const loadCfg = React.useCallback(async () => {
        try {
          const res = await fetch(CONFIG_URL, { cache: 'no-store' });
          const body = await res.json();
          if (res.ok && !body.error) { setCfg(body); }
        } catch { /* 瞬时失败：下次开菜单再试 */ }
      }, []);
      const loadOnline = React.useCallback(async () => {
        try {
          const res = await fetch(STATUS_URL, { cache: 'no-store' });
          const body = await res.json();
          const devices = body?.snapshot?.relay?.devices;
          if (res.ok && Array.isArray(devices)) {
            setOnline(new Map(devices.map((d) => [d.id, d.online === true])));
          } else setOnline(null);
        } catch { setOnline(null); }
      }, []);

      React.useEffect(() => {
        injectStyleOnce();
        void loadCfg();
      }, [loadCfg]);

      // 座位解析：组件挂在 shell.overlay（常驻），从 document 找会话头部的标签行。
      // 有标签行 → portal 进标签行右端（红框处）；没有（空白会话）→ 固定右上角兜底。
      // 会话切换/视图增减都会换 DOM，用 MutationObserver 兜底重找。
      React.useEffect(() => {
        const resolve = () => {
          const header = document.querySelector('header[data-window-drag]');
          const tabs = header ? header.querySelector('[data-conversation-tabs]') : null;
          const row = tabs ? tabs.parentElement : null;
          setSeat((prev) => (prev === row ? prev : (row || null)));
        };
        resolve();
        const obs = new MutationObserver(resolve);
        obs.observe(document.body, { childList: true, subtree: true });
        return () => obs.disconnect();
      }, [cfg]);

      // 打开菜单：拉最新 config + 在线态，算 fixed 坐标（右缘对齐按钮，向下开）。
      // 桥接管时在线态/当前节点以壳页 state() 为权威（probe 每 30s 刷新）。
      const openMenu = () => {
        if (switching) return;
        setError(null);
        void loadCfg();
        const bridge = shellBridge();
        if (bridge) {
          const st = bridge.state();
          setVisible(st.current);
          if (Array.isArray(st.devices)) setOnline(new Map(st.devices.map((d) => [d.id, d.online === true])));
        } else {
          void loadOnline();
        }
        const r = btnRef.current?.getBoundingClientRect();
        if (r) {
          // 卡片宽度随内容收缩（2026-11-10 用户：卡片太宽）。fixed + width:auto
          // 收缩到最长 deviceId 行；右缘对齐按钮右缘，CSS max-width 兜视口。
          setPos({ top: r.bottom + 6, right: r.right });
        }
        setOpen(true);
      };
      React.useEffect(() => {
        if (!open) return;
        const onDocDown = (ev) => {
          if (menuRef.current?.contains(ev.target) || btnRef.current?.contains(ev.target)) return;
          setOpen(false);
        };
        const onKey = (ev) => { if (ev.key === 'Escape') { setOpen(false); btnRef.current?.focus(); } };
        const onScroll = () => setOpen(false);
        document.addEventListener('pointerdown', onDocDown, true);
        document.addEventListener('keydown', onKey);
        window.addEventListener('scroll', onScroll, true);
        window.addEventListener('resize', onScroll);
        return () => {
          document.removeEventListener('pointerdown', onDocDown, true);
          document.removeEventListener('keydown', onKey);
          window.removeEventListener('scroll', onScroll, true);
          window.removeEventListener('resize', onScroll);
        };
      }, [open]);

      const onPick = async (node) => {
        const isActive = visible ? node.deviceId === visible : node.active;
        if (switching || isActive) { if (isActive) setOpen(false); return; }
        setSwitching(node.id); setError(null);
        // 唯一链路：POST relay default-node。壳页 SSE 收到变化后切 iframe
        // （0 重载）；不再双写 config、不再整页 reload。
        const out = await switchNode(node.deviceId);
        if (!out.ok) { setError(out.error); }
        else {
          setOpen(false);
          // 选中态与可见 iframe 唯一由壳页 SSE 跟随并 subscribe 回来；
          // 这里不本地 setVisible，避免「按钮先变、iframe 后切」的双源不一致。
          // 非桥环境（桌面直连/旧 relay）无 SSE，读回 config 对齐状态。
          if (!shellBridge()) void loadCfg();
        }
        setSwitching(null);
      };

      const cfgNodes = cfg?.nodes || [];
      const bridge = shellBridge();
      // 桥环境 cfg 未就绪（新 iframe 冷启动）时，菜单列表用壳页 probe 的
      // devices 兜底，保证切换入口始终可用；deviceId 即 id。
      const nodes = cfgNodes.length > 0 ? cfgNodes
        : (bridge ? (bridge.state().devices || []).map((d) => ({ id: d.id, deviceId: d.id, label: d.id, active: d.id === visible })) : []);
      if (cfg !== null && cfgNodes.length === 0 && nodes.length === 0) return null; // 没配节点：不占位
      // 桥环境（壳页 iframe）：选中态唯一来自壳页 state()/subscribe() 的 visible，
      // 不依赖 cfg（新 iframe 冷启动时 cfg 未就绪，label 不该停在 '…'）。
      // 无桥环境回退 config.active。
      const active = bridge && visible
        ? (nodes.find((n) => n.deviceId === visible) || { deviceId: visible })
        : (nodes.find((n) => n.active) || null);
      // 红圈样式：触发器与菜单都显示 deviceId（不是 label）。
      const label = cfg === null
        ? (bridge && visible ? visible : '…')
        : (active ? active.deviceId : t('node.none'));

      const btn = h('button', {
        ref: btnRef,
        type: 'button',
        className: 'rar-nswitch__btn',
        'aria-haspopup': 'menu',
        'aria-expanded': open,
        'aria-label': t('ns.aria'),
        title: t('ns.aria'),
        disabled: switching !== null,
        onClick: () => { if (open) setOpen(false); else openMenu(); },
      },
      h('span', { className: 'rar-nswitch__label' }, label),
      icon('chevron', 12));

      const menu = open && pos ? ReactDOM.createPortal(
        h('div', {
          ref: menuRef, className: 'rar-nmenu', role: 'menu', 'aria-label': t('ns.aria'),
          style: { top: pos.top, right: Math.max(8, window.innerWidth - pos.right) },
        },
        nodes.map((node) => {
          const obs = online && online.has(node.deviceId) ? online.get(node.deviceId) : null;
          const busy = switching === node.id;
          const isActive = active && active.deviceId === node.deviceId;
          // 红圈样式（2026-11-10 用户给定）：一行 = 状态色点 + deviceId + 行尾
          // 选中勾。无标题、无 label 副行、无胶囊；在线态只由色点承载
          // （绿=在线 红=离线 灰=未观测），aria-label 带语义词不靠颜色猜。
          const dotTone = obs === true ? 'on' : obs === false ? 'off' : 'unknown';
          const dotWord = obs === true ? t('node.online') : obs === false ? t('node.offline') : t('node.unknown');
          return h('button', {
            key: node.id, type: 'button', role: 'menuitem',
            className: 'rar-nmenu__item', disabled: switching !== null,
            'aria-current': isActive ? 'true' : undefined,
            'aria-label': `${node.deviceId} · ${dotWord}`,
            onClick: () => { void onPick(node); },
          },
          h('span', { className: `rar-nmenu__dot rar-nmenu__dot--${dotTone}`, 'aria-hidden': 'true' }),
          h('span', { className: 'rar-nmenu__name' }, node.deviceId),
          busy ? h('span', { className: 'rar-nmenu__busy', 'aria-label': t('node.switching') }, icon('refresh', 16))
            : isActive ? h('span', { className: 'rar-nmenu__check', 'aria-label': t('config.active') }, icon('check', 18))
              : null);
        }),
        error !== null ? h('div', { className: 'rar-nmenu__err', role: 'alert' }, error) : null),
        document.body) : null;

      // 组件挂在 shell.overlay（常驻，空白会话也在）。有标签行 → 按钮 portal 到
      // 行尾（红框位置）；没有 → 固定右上角兜底（头部空白区，不挡内容）。
      const intoTabs = seat && seat.isConnected;
      const fallbackBtn = intoTabs ? null : h('span', {
        className: 'rar-nswitch-float',
        style: { position: 'fixed', top: 6, right: 8, zIndex: 30 },
      }, btn);
      return h('span', { ref: rootRef, className: 'rar-nswitch', 'data-rar-nswitch': '' },
        fallbackBtn,
        intoTabs ? ReactDOM.createPortal(btn, seat) : null,
        menu);
    }

    /** Full Settings section: status + node switching + relay entrances + facts. */
    function RemoteAccessSection(props) {
      const t = props.t;
      const [status, setStatus] = React.useState(null);
      const [statusError, setStatusError] = React.useState(null);
      const [cfg, setCfg] = React.useState(null);
      const [cfgError, setCfgError] = React.useState(null);
      const [refreshing, setRefreshing] = React.useState(false);
      const [switching, setSwitching] = React.useState(false);
      // C9：入场动画只在"新内容出现"时挂；刷新完成后的重绘不挂（那是数据更新）。
      const [animate, setAnimate] = React.useState(true);

      const loadConfig = React.useCallback(async () => {
        try {
          const res = await fetch(CONFIG_URL, { cache: 'no-store' });
          const body = await res.json();
          // 与 loadStatus 同式：JSON null 不算成功（null.error 会炸成异常）。
          if (!res.ok || !body || body.error) { setCfg(null); setCfgError((body && body.error) || `HTTP ${res.status}`); }
          else { setCfg(body); setCfgError(null); }
        } catch (e) { setCfg(null); setCfgError(String(e && e.message || e)); }
      }, []);

      const loadStatus = React.useCallback(async (opts = {}) => {
        setRefreshing(true);
        try {
          const res = await fetch(STATUS_URL, { cache: 'no-store' });
          const body = await res.json();
          // body 可能是 JSON null（proxy 空回复/上游 204 空体）：null.error 会把
          // 「诚实显示未检查」炸成 fetch 异常。null 不是成功也不是失败 → 归 error。
          if (!res.ok || !body || body.error) { setStatus(null); setStatusError((body && body.error) || `HTTP ${res.status}`); }
          else { setStatus(body); setStatusError(null); }
        } catch (e) { setStatus(null); setStatusError(String(e && e.message || e)); }
        finally {
          setRefreshing(false);
          setAnimate(opts.animate !== false ? true : false);
        }
      }, []);

      React.useEffect(() => {
        injectStyleOnce();
        installPressFeedback();
        void loadConfig(); void loadStatus();
      }, [loadConfig, loadStatus]);

      const onSwitch = async (patch) => {
        if (switching) return;
        setSwitching(true); setCfgError(null);
        // 唯一链路：节点切换走 switchNode（POST relay default-node，壳页 SSE 跟随）；
        // 入口切换（activeEndpoint）不改 default-node，仍走 host config 持久化。
        if (patch.nodeId) {
          const out = await switchNode(patch.nodeId);
          if (!out.ok) setCfgError(out.error);
          else { await loadConfig(); await loadStatus({ animate: true }); }
        } else {
          try {
            const res = await fetch(CONFIG_URL, {
              method: 'POST', headers: { 'content-type': 'application/json' },
              body: JSON.stringify(patch),
            });
            const body = await res.json();
            if (!res.ok || body.error) setCfgError(body.error || `HTTP ${res.status}`);
            else { setCfg(body); await loadStatus({ animate: true }); }
          } catch (e) { setCfgError(String(e && e.message || e)); }
        }
        setSwitching(false);
      };

      // ── 数据面安装（dsh-tunnel）：状态 + 安装/卸载。状态永远"胶囊 + 词"，
      // 动作一律图形按钮（用户既定规则）；busy 不把文案换成「…」。 ──
      const TI_URL = API_BASE + '/api/remote-access/tunnel-install';
      const [tiStatus, setTiStatus] = React.useState(null);
      const [tiBusy, setTiBusy] = React.useState(false);
      const [tiMsg, setTiMsg] = React.useState(null);   // {tone:'good'|'bad', text}
      const [tiDeviceId, setTiDeviceId] = React.useState('');
      const loadTiStatus = React.useCallback(async () => {
        try {
          const res = await fetch(TI_URL, { cache: 'no-store' });
          const text = await res.text();
          let body = null;
          try { body = JSON.parse(text); } catch { /* host route absent → plain 404 text */ }
          if (!res.ok || body === null || body.error) {
            setTiStatus(null);
            setTiMsg({ tone: 'bad', text: body?.error || (body === null ? `host route unavailable (HTTP ${res.status}); restart dsh-web to activate` : `HTTP ${res.status}`) });
            return;
          }
          setTiStatus(body); setTiMsg(null);
        } catch (e) { setTiStatus(null); setTiMsg({ tone: 'bad', text: String(e && e.message || e) }); }
      }, []);
      React.useEffect(() => { void loadTiStatus(); }, [loadTiStatus]);
      const tiAction = async (payload) => {
        if (tiBusy) return;
        setTiBusy(true); setTiMsg(null);
        try {
          const res = await fetch(TI_URL, {
            method: 'POST', headers: { 'content-type': 'application/json' },
            body: JSON.stringify(payload),
          });
          const text = await res.text();
          let body = null;
          try { body = JSON.parse(text); } catch { /* host route absent → plain 404 text */ }
          if (!res.ok || body === null || body.error) {
            setTiMsg({ tone: 'bad', text: body?.error || (body === null ? `host route unavailable (HTTP ${res.status}); restart dsh-web to activate` : `HTTP ${res.status}`) });
            return;
          }
          setTiMsg({ tone: 'good', text: `${t('ti.ok')}: ${body.unitPath || body.scriptPath || body.path || body.removed || ''}` });
          await loadTiStatus(); await loadStatus({ animate: false });
        } catch (e) { setTiMsg({ tone: 'bad', text: String(e && e.message || e) }); }
        finally { setTiBusy(false); }
      };

      const tiBlock = h('div', { key: 'ti', className: 'rar-block' },
        h('h3', { className: 'rar-secTitle' }, t('ti.title')),
        h('p', { className: 'rar-note' }, t('ti.note')),
        h('div', { className: 'rar-group' },
          tiStatus === null
            ? h('div', { className: 'rar-row' }, h('span', { className: 'rar-row__value' }, '…'))
            : [
              h('div', { key: 'bin', className: 'rar-row' },
                h('span', { className: 'rar-row__label' }, t('ti.binary')),
                h('span', { className: 'rar-row__value rar-row__value--pills' },
                  pill(tiStatus.installed ? 'good' : '', tiStatus.installed ? t('ti.installed') : t('ti.absent')),
                  tiStatus.installed ? h('span', { className: 'rar-row__value-text rar-row__value--mono' }, `${tiStatus.binPath} (${tiStatus.size ?? '?'} B)`) : null)),
              h('div', { key: 'svc', className: 'rar-row' },
                h('span', { className: 'rar-row__label' }, t('ti.service')),
                h('span', { className: 'rar-row__value rar-row__value--pills' },
                  pill(tiStatus.serviceState === 'active' || tiStatus.serviceState === 'running' ? 'good' : tiStatus.serviceState === 'absent' ? '' : 'warn',
                    String(tiStatus.serviceState ?? 'unknown')),
                  tiStatus.unitManaged === true ? pill('', t('ti.managed'))
                    : tiStatus.unitManaged === false ? pill('warn', t('ti.unmanaged')) : null)),
              h('div', { key: 'ch', className: 'rar-row' },
                h('span', { className: 'rar-row__label' }, 'channel'),
                h('span', { className: 'rar-row__value rar-row__value--mono' }, String(tiStatus.channel ?? ''))),
            ],
          h('div', { key: 'act', className: 'rar-row' },
            h('span', { className: 'rar-row__label' }, t('ti.deviceId')),
            h('span', { className: 'rar-row__value rar-acts-r' },
              h('input', {
                value: tiDeviceId, placeholder: t('ti.deviceIdPh'),
                onChange: (ev) => setTiDeviceId(ev.target.value),
                'aria-label': t('ti.deviceId'),
                className: 'rar-input',
              }),
              h('button', {
                onClick: () => tiAction({ deviceId: tiDeviceId.trim() }), disabled: tiBusy,
                className: `rar-iconbtn${tiBusy ? ' rar-iconbtn--busy' : ''}`,
                'aria-label': tiBusy ? t('ti.installing') : t('ti.install'), title: tiBusy ? t('ti.installing') : t('ti.install'),
              }, icon('swap')),
              h('button', {
                onClick: () => tiAction({ action: 'uninstall' }), disabled: tiBusy,
                className: 'rar-iconbtn', 'aria-label': t('ti.uninstall'), title: t('ti.uninstall'),
              }, icon('refresh')))),
          tiMsg !== null ? h('div', { key: 'msg', className: `rar-banner rar-banner--${tiMsg.tone === 'good' ? 'ok' : 'bad'}` },
            tiMsg.text) : null,
          h('div', { key: 'hint', className: 'rar-row' },
            h('span', { className: 'rar-row__value rar-help' }, t('ti.installHint')))));

      // ── 扫码配对：QR + 倒计时 + 已配对设备列表（撤销带确认）──
      // 一次性码由 relay 铸造；这里只展示。QR 载荷不含任何长效凭据。
      const PAIR_URL = API_BASE + '/api/remote-access/pair';
      const [pairQr, setPairQr] = React.useState(null);       // {svg,payload,expiresAt}
      const [pairBusy, setPairBusy] = React.useState(false);
      const [pairMsg, setPairMsg] = React.useState(null);     // {tone,text}
      const [pairDevices, setPairDevices] = React.useState(null);
      const [pairSupported, setPairSupported] = React.useState(null); // null=未知 false=relay未开
      const [pairNow, setPairNow] = React.useState(Date.now());
      const [pairConfirm, setPairConfirm] = React.useState(''); // deviceId awaiting confirm

      const pairCall = React.useCallback(async (action, extra = {}) => {
        const res = await fetch(PAIR_URL, {
          method: 'POST', headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ action, ...extra }),
        });
        const text = await res.text();
        let body = null;
        try { body = JSON.parse(text); } catch { /* route absent */ }
        if (body === null) throw new Error(`host route unavailable (HTTP ${res.status}); restart dsh-web to activate`);
        if (body.supported === false) { setPairSupported(false); return body; }
        setPairSupported(true);
        if (!res.ok || body.error) throw new Error(body.error || `HTTP ${res.status}`);
        return body;
      }, []);

      const loadPairDevices = React.useCallback(async () => {
        try {
          const body = await pairCall('devices');
          if (body.supported !== false) setPairDevices(Array.isArray(body.devices) ? body.devices : []);
        } catch (e) { setPairMsg({ tone: 'bad', text: String(e && e.message || e) }); }
      }, [pairCall]);

      React.useEffect(() => { void loadPairDevices(); }, [loadPairDevices]);
      // 倒计时秒针：只在有活跃 QR 时走。
      React.useEffect(() => {
        if (!pairQr) return undefined;
        const timer = setInterval(() => setPairNow(Date.now()), 1000);
        return () => clearInterval(timer);
      }, [pairQr]);

      const mintPair = async () => {
        if (pairBusy) return;
        setPairBusy(true); setPairMsg(null);
        try {
          const body = await pairCall('mint');
          if (body.supported === false) { setPairMsg({ tone: 'bad', text: t('pair.unsupported') }); return; }
          setPairQr({ svg: body.svg, payload: body.payload, expiresAt: body.expiresAt });
          setPairNow(Date.now());
        } catch (e) { setPairMsg({ tone: 'bad', text: String(e && e.message || e) }); }
        finally { setPairBusy(false); }
      };

      const revokePair = async (deviceId) => {
        if (pairBusy) return;
        setPairBusy(true); setPairMsg(null);
        try {
          const body = await pairCall('revoke', { deviceId });
          setPairConfirm('');
          setPairMsg({ tone: body.warning ? 'bad' : 'good', text: body.warning || t('pair.revoked') });
          await loadPairDevices();
        } catch (e) { setPairMsg({ tone: 'bad', text: String(e && e.message || e) }); }
        finally { setPairBusy(false); }
      };

      const pairRemaining = pairQr ? Math.max(0, Math.ceil((pairQr.expiresAt - pairNow) / 1000)) : 0;
      const pairBlock = h('div', { key: 'pair', className: 'rar-block' },
        pairSupported === false
          ? h('div', { className: 'rar-banner rar-banner--bad' }, t('pair.unsupported'))
          : h('div', { className: 'rar-group' },
            pairQr && pairRemaining > 0
              ? h('div', { key: 'qr', className: 'rar-qr' },
                // SVG 由 host 用 vendored MIT qrcode-generator 生成；载荷只有
                // 一次性码。dangerouslySetInnerHTML 的输入源是本插件 host，非用户。
                h('div', { className: 'rar-qr__box', 'aria-label': t('pair.qrAlt'), dangerouslySetInnerHTML: { __html: pairQr.svg } }),
                h('div', { className: 'rar-help' }, `${t('pair.expiresIn')}: ${pairRemaining}s`))
              : pairQr ? h('div', { key: 'noqr', className: 'rar-qr' },
                h('div', { className: 'rar-qr__box rar-qr__box--expired', 'aria-label': t('pair.qrAlt'), dangerouslySetInnerHTML: { __html: pairQr.svg } }),
                h('div', { className: 'rar-help' }, t('pair.expired'))) : null,
            h('div', { key: 'act', className: 'rar-row' },
              h('span', { className: 'rar-row__label' }, t('pair.code')),
              h('span', { className: 'rar-row__value rar-acts-r' },
                h('button', {
                  onClick: mintPair, disabled: pairBusy,
                  className: `rar-iconbtn${pairBusy ? ' rar-iconbtn--busy' : ''}`,
                  'aria-label': t('pair.mint'), title: t('pair.mint'),
                }, icon('refresh')),
                pairQr ? h('button', {
                  onClick: () => { try { void navigator.clipboard?.writeText(pairQr.payload); setPairMsg({ tone: 'good', text: t('pair.copied') }); } catch { setPairMsg({ tone: 'bad', text: t('pair.copyFail') }); } },
                  className: 'rar-iconbtn', 'aria-label': t('pair.copy'), title: t('pair.copy'),
                }, icon('copy')) : null)),
            Array.isArray(pairDevices) && pairDevices.length > 0
              ? h('div', { key: 'devs' }, pairDevices.map((d) => h('div', { key: d.deviceId, className: 'rar-row' },
                h('span', { className: 'rar-row__label' }, String(d.name || d.deviceId)),
                h('span', { className: 'rar-row__value rar-acts-r rar-acts-r--center' },
                  h('span', { className: 'rar-help' }, d.lastSeenAt ? new Date(d.lastSeenAt).toLocaleString() : ''),
                  pairConfirm === d.deviceId
                    ? [
                      h('button', { key: 'yes', onClick: () => revokePair(d.deviceId), disabled: pairBusy, className: 'rar-iconbtn', 'aria-label': t('pair.confirmYes'), title: t('pair.confirmYes') }, icon('check')),
                      h('button', { key: 'no', onClick: () => setPairConfirm(''), className: 'rar-iconbtn', 'aria-label': t('pair.confirmNo'), title: t('pair.confirmNo') }, icon('close')),
                    ]
                    : h('button', {
                      onClick: () => setPairConfirm(d.deviceId), disabled: pairBusy,
                      className: 'rar-iconbtn', 'aria-label': t('pair.revoke'), title: t('pair.revoke'),
                    }, icon('close'))))))
              : Array.isArray(pairDevices) ? h('div', { key: 'empty', className: 'rar-row' }, h('span', { className: 'rar-row__value rar-help' }, t('pair.noDevices'))) : null,
            pairMsg !== null ? h('div', { key: 'msg', className: `rar-banner rar-banner--${pairMsg.tone === 'good' ? 'ok' : 'bad'}` }, pairMsg.text) : null));

      // 刷新：图形按钮（圆形箭头）放在「状态」标题右侧。busy 时图标旋转 +
      // aria-label 切到「正在检查…」，保留可读状态而不是把文案换成一个「…」。
      const refreshLabel = refreshing ? t('status.refreshing') : t('status.refresh');
      return h('div', { className: 'rar-section' },
        h('div', { className: 'rar-secHead' },
          h('h3', { className: 'rar-secTitle' }, t('section.statusTitle')),
          h('button', {
            onClick: () => loadStatus({ animate: false }),
            disabled: refreshing,
            className: `rar-iconbtn${refreshing ? ' rar-iconbtn--busy' : ''}`,
            'aria-label': refreshLabel,
            title: refreshLabel,
          }, icon('refresh'))),
        h('div', { className: 'rar-group' },
          h(StatusBlock, { t, status, error: statusError, animate, cfgLoaded: cfg !== null })),
        h('h3', { className: 'rar-secTitle' }, t('section.nodesTitle')),
        h('p', { className: 'rar-note' }, t('section.nodesNote')),
        h(NodeList, { t, cfg, switching, error: cfgError, onSwitch }),
        h('h3', { className: 'rar-secTitle' }, t('section.endpointsTitle')),
        h('p', { className: 'rar-note' }, t('section.endpointsNote')),
        h(EndpointList, { t, cfg, switching, onSwitch }),
        pairBlock,
        tiBlock,
        // C3 诚实性 + 密度：四个配置事实合并成一行点分隔（缺 = 「未上报」），
        // 编辑处单独一行（它是可操作的路径，和事实不同类）。
        cfg !== null ? h('div', { className: 'rar-help rar-facts' },
          h('div', {}, [
            `${t('config.platform')}: ${cfg.platform ? String(cfg.platform) : t('status.notReported')}`,
            `${t('config.allowRestart')}: ${typeof cfg.allowRestart === 'boolean' ? String(cfg.allowRestart) : t('status.notReported')}`,
            `${t('config.logTailLines')}: ${cfg.logTailLines != null ? String(cfg.logTailLines) : t('status.notReported')}`,
          ].join(' · ')),
          h('div', {}, t('config.editHint'), ' ', h('code', {}, cfg.stateFile || t('status.notReported')))) : null);
    }

    return {
      inject: ['slots', 'locale'],
      apply(ctx) {
        ctx.effect(() => ctx.locale.register(NS, {
          zh: {
            'section.nav': '远程访问',
            'section.statusTitle': '状态',
            'section.nodesTitle': '被控节点',
            'section.nodesNote': '节点是被控制的机器：每台机器跑自己的 dsh-tunnel（不同 device-id），relay 用 /d/<deviceId>/ 路由。切换只改变观测与入口，不重启任何服务。',
            'section.endpointsTitle': 'Relay 入口',
            'section.endpointsNote': '入口是同一 relay 的不同访问路径（域名 / IP 直连），只影响状态探测与重启操作指向，不改变被控节点。',
            'status.endpoint': '当前入口', 'status.node': '当前节点',
            'status.relay': 'Relay',
            'status.verdict': '结论', 'status.idle': '尚未检查',
            'status.idleDetail': '还没有检查过；这一态不兜底任何字段。',
            'status.idleOnlyStatus': '配置已经读到，只有状态请求还没回 —— 不预填任何状态字段。',
            'status.refresh': '刷新状态', 'status.refreshing': '正在检查…',
            'status.deviceOnline': '设备在线', 'status.deviceOffline': '设备离线',
            'status.unreachable': '不可达',
            'status.services': '服务单元',
            'status.servicesDetail': '单元明细',
            'status.mismatch': '（状态不一致）',
            'status.notReported': '未上报',
            'status.failPrefix': '状态请求失败：', 'status.failSuffix': ' —— 原文照客户端拼法（body.error 或 HTTP 状态码），翻成中文会让人按错方向排查。',
            'node.online': '在线', 'node.offline': '离线', 'node.unknown': '未知',
            'node.none': '未配置节点', 'node.empty': '在插件配置的 nodes 中添加；当前选择持久化于状态文件。',
            'node.switch': '切换到此节点', 'node.switching': '切换中…', 'node.copy': '复制地址', 'node.urlLabel': '地址',
            'config.loading': '正在加载配置…', 'config.none': '未配置任何端点',
            'config.empty': '在插件配置的 endpoints 中添加；入口是同一 relay 的不同访问路径，只影响探测与重启指向。',
            'config.active': '使用中', 'config.switch': '切换到此入口', 'config.switching': '切换中…',
            'config.caFile': '私有 CA', 'config.platform': '平台',
            'config.editHint': '节点/入口列表在 cordis.patch.yml 中编辑；当前选择持久化于',
            'config.allowRestart': '允许重启', 'config.logTailLines': '日志尾行数',
            'ti.title': '数据面安装（dsh-tunnel）',
            'ti.note': '在这台机器上下载并安装 dsh-tunnel（SHA-256 校验，systemd 用户单元 / Windows 服务脚本）。总闸在插件配置 tunnelInstall.allowInstall。',
            'ti.binary': '二进制', 'ti.absent': '未安装', 'ti.installed': '已安装',
            'ti.service': '服务', 'ti.managed': '本插件托管', 'ti.unmanaged': '手动管理（本插件不覆盖）',
            'ti.install': '安装 / 升级', 'ti.installing': '安装中…', 'ti.uninstall': '卸载服务', 'ti.uninstalling': '卸载中…',
            'ti.deviceId': '设备 ID', 'ti.deviceIdPh': '如 office-pc（必填）',
            'ti.installHint': '安装会从当前 Relay 的 update 通道下载二进制并注册服务；Windows 生成脚本需管理员运行。',
            'ti.disabled': '未开启：tunnelInstall.allowInstall=false', 'ti.ok': '完成',
            'pair.code': '配对码', 'pair.mint': '生成 / 刷新二维码', 'pair.copy': '复制配对载荷',
            'pair.copied': '已复制', 'pair.copyFail': '复制失败',
            'pair.expiresIn': '剩余', 'pair.expired': '码已过期，点刷新重新生成',
            'pair.qrAlt': '手机配对二维码',
            'pair.revoke': '撤销此设备', 'pair.confirmYes': '确认撤销', 'pair.confirmNo': '取消',
            'pair.revoked': '已撤销；该设备下次请求即被拒', 'pair.noDevices': '还没有已配对的设备',
            'pair.unsupported': '此 relay 未启用设备配对（PhoneDevicesFile 未配置）；升级 relay 并在配置中开启后可用。',
            'ns.aria': '切换节点',
          },
          en: {
            'section.nav': 'Remote access',
            'section.statusTitle': 'Status',
            'section.nodesTitle': 'Controlled nodes',
            'section.nodesNote': 'A node is a controlled machine: each runs its own dsh-tunnel with a distinct device id, and the relay routes /d/<deviceId>/. Switching only changes observation and entry URL; it restarts nothing.',
            'section.endpointsTitle': 'Relay entrances',
            'section.endpointsNote': 'Entrances are different access paths to the same relay (domain / direct IP). They affect probing and restart targets, not which node is controlled.',
            'status.endpoint': 'Active entrance', 'status.node': 'Active node',
            'status.relay': 'Relay',
            'status.verdict': 'Verdict', 'status.idle': 'Not checked yet',
            'status.idleDetail': 'No check has run yet; no field is filled in on this state.',
            'status.idleOnlyStatus': 'Configuration is loaded; only the status request has not returned yet — no field is pre-filled.',
            'status.refresh': 'Refresh status', 'status.refreshing': 'Checking…',
            'status.deviceOnline': 'device online', 'status.deviceOffline': 'device offline',
            'status.unreachable': 'unreachable',
            'status.services': 'Service units',
            'status.servicesDetail': 'Unit details',
            'status.mismatch': ' (states differ)',
            'status.notReported': 'not reported',
            'status.failPrefix': 'Status request failed: ', 'status.failSuffix': ' — shown verbatim from the client (body.error or HTTP status); translating it would send you debugging the wrong thing.',
            'node.online': 'online', 'node.offline': 'offline', 'node.unknown': 'unknown',
            'node.none': 'no node configured', 'node.empty': 'Add them under nodes in the plugin config; the selection persists in a state file.',
            'node.switch': 'Switch to this node', 'node.switching': 'Switching…', 'node.copy': 'Copy URL', 'node.urlLabel': 'URL',
            'config.loading': 'Loading configuration…', 'config.none': 'No endpoints configured',
            'config.empty': 'Add them under endpoints in the plugin config; entrances are different access paths to the same relay and only affect probing and restart targets.',
            'config.active': 'Active', 'config.switch': 'Switch to this entrance', 'config.switching': 'Switching…',
            'config.caFile': 'Private CA', 'config.platform': 'Platform',
            'config.editHint': 'Edit nodes/endpoints in cordis.patch.yml; selection persists at',
            'config.allowRestart': 'Allow restart', 'config.logTailLines': 'Log tail lines',
            'ti.title': 'Data plane install (dsh-tunnel)',
            'ti.note': 'Download and install dsh-tunnel on this machine (SHA-256 verified, systemd user unit / Windows service script). Master switch: tunnelInstall.allowInstall in the plugin config.',
            'ti.binary': 'Binary', 'ti.absent': 'not installed', 'ti.installed': 'installed',
            'ti.service': 'Service', 'ti.managed': 'managed by this plugin', 'ti.unmanaged': 'manually managed (this installer will not overwrite)',
            'ti.install': 'Install / upgrade', 'ti.installing': 'Installing…', 'ti.uninstall': 'Uninstall service', 'ti.uninstalling': 'Uninstalling…',
            'ti.deviceId': 'Device ID', 'ti.deviceIdPh': 'e.g. office-pc (required)',
            'ti.installHint': 'Install downloads the binary from the active relay update channel and registers the service; on Windows run the generated script as Administrator.',
            'ti.disabled': 'disabled: tunnelInstall.allowInstall=false', 'ti.ok': 'done',
            'pair.code': 'Pairing code', 'pair.mint': 'Mint / refresh QR', 'pair.copy': 'Copy pairing payload',
            'pair.copied': 'Copied', 'pair.copyFail': 'Copy failed',
            'pair.expiresIn': 'Expires in', 'pair.expired': 'Code expired; refresh to mint a new one',
            'pair.qrAlt': 'Phone pairing QR code',
            'pair.revoke': 'Revoke this device', 'pair.confirmYes': 'Confirm revoke', 'pair.confirmNo': 'Cancel',
            'pair.revoked': 'Revoked; the device is refused on its next request', 'pair.noDevices': 'No paired devices yet',
            'pair.unsupported': 'This relay has device pairing disabled (PhoneDevicesFile unset); upgrade the relay and enable it in config.',
            'ns.aria': 'Switch node',
          },
        }), 'remote-access: dictionaries');

        // Settings section (owner of status, node switching and configuration).
        ctx.slots.inject('settings.section', () => ctx.slots.register({
          name: 'settings.section',
          id: 'remote-access',
          order: 30,
          label: () => ctx.locale.bind(NS)('section.nav'),
          locale: NS,
        }, RemoteAccessSection));

        // 节点下拉开关：shell.overlay 是常驻 frame 级浮层槽（空白会话也在），
        // 组件自己 portal 到会话头部标签行右端（红框处），无标签行时固定右上角。
        // 用 overlay 而非 session.header.utilities：后者在空白会话隐藏 chrome 时
        // 不挂载，会导致开关消失、无法切回有会话的节点。
        ctx.slots.inject('shell.overlay', () => ctx.slots.register({
          name: 'shell.overlay',
          id: 'remote-access-nodes',
          order: 100,
          locale: NS,
        }, (props) => h(NodeSwitch, { t: ctx.locale.bind(NS) })));

        // composer.dock 徽章已移除（2026-11-05 用户定）：手机上它是常驻噪音
        // （idle 态「点此检查」还换行成两行），而它能给的信息设置页都有。
        // 状态的唯一入口 = 设置页「远程访问」分区。dock.* locale 键一并删除，
        // 留着没人引用的键等于撒谎。
      },
    };
  },
});
