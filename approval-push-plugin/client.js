window.__ModuleLoader__.load({
  id: '@local/dsh-approval-push',
  factory(require) {
    const React = require('react');
    const h = React.createElement;
    const NS = 'approvalPush';

    // 桌面侧最小面板：说明 + 当前配置事实。配对/撤销在「远程访问」分区，
    // 这里不重复造轮子。手机端的通知由原生层处理，这个面板只是桌面可见性。
    function ApprovalPushSection(props) {
      const t = props.t;
      return h('div', { className: 'aps-section' },
        h('h3', {}, t('title')),
        h('p', {}, t('note')),
        h('ul', {},
          h('li', {}, t('rule.claim')),
          h('li', {}, t('rule.failClosed')),
          h('li', {}, t('rule.privacy')),
          h('li', {}, t('rule.keepalive'))));
    }

    return {
      inject: ['slots', 'locale', 'uiWorkspace'],
      apply(ctx) {
        ctx.effect(() => ctx.locale.register(NS, {
          zh: {
            'title': '审批推送到手机',
            'note': 'agent 请求审批、提问或跑完一轮时，已配对的手机会收到通知卡片；审批可允许一次/拒绝，提问可直接在通知上选答案。',
            'rule.claim': '只有手机在线订阅时才接管审批；手机不在，审批照常留在桌面。',
            'rule.failClosed': '接管后无人应答＝拒绝（超时自动拒绝，沉默不是同意）；提问超时按超时处理，不会伪造答案。',
            'rule.privacy': '提问卡会显示问题与选项（可在设置里关）；多题、多选或选项过多时请进 App 作答。',
            'rule.keepalive': 'App 在前台服务常驻时可继续收卡片；主动划掉 App 或系统极端省电仍会中断。',
          },
          en: {
            'title': 'Approval push to phone',
            'note': 'When an agent asks for approval, asks a question, or finishes a turn, paired phones receive a notification card. Approvals offer allow-once / reject; questions can be answered right on the notification.',
            'rule.claim': 'Approvals are claimed only while a phone is subscribed; otherwise they stay on the desktop.',
            'rule.failClosed': 'A claimed ask nobody answers is rejected on timeout — silence is never consent. A question times out instead of fabricating an answer.',
            'rule.privacy': 'Question cards show the question text and options (configurable); multi-question or multi-select asks must be answered in the app.',
            'rule.keepalive': 'A foreground service keeps cards flowing while the app is alive; swiping the app away or aggressive battery savers still interrupt it.',
          },
        }), 'approval-push: dictionaries');
        // 通知点击深链：GUI 读 #session=<id>（或 ?session=）→ 打开对应会话。
        // 冷启动由 setup 页把 hash 拼在导航 URL 上；热启动由 MainActivity
        // 改 location.hash 触发 hashchange，不重载页面。
        ctx.effect(() => {
          let disposed = false
          const clearHash = () => {
            try {
              const hash = window.location.hash || ''
              const search = window.location.search
                .replace(/([?&])session=[^&]*/g, '$1')
                .replace(/[?&]$/, '')
              const restHash = hash.replace(/([#&])session=[^&]*/g, '$1').replace(/^#$/, '')
              window.history.replaceState(null, '', window.location.pathname + search + restHash)
            } catch (_) {}
          }
          // openSession() 走 sessions.retain()：会话目录还没加载完时会抛。
          // 挂载瞬间通常还没加载完，所以指数退避重试几次；成功后再清 hash。
          const attempt = (id, tries) => {
            if (disposed) return
            try {
              ctx.uiWorkspace.openSession(id)
              clearHash()
            } catch (_) {
              if (tries > 0) setTimeout(() => attempt(id, tries - 1), 800 * (5 - tries))
            }
          }
          const openFromLocation = () => {
            try {
              const hash = window.location.hash || ''
              const fromHash = /(?:^#|&)session=([^&]+)/.exec(hash)
              const fromQuery = new URLSearchParams(window.location.search).get('session')
              const raw = fromHash ? fromHash[1] : fromQuery
              if (!raw) return
              const id = decodeURIComponent(raw)
              if (!id) return
              attempt(id, 4)
            } catch (_) {
              // 深链失败不阻塞 GUI：用户仍可在侧栏手动选会话。
            }
          }
          openFromLocation()
          window.addEventListener('hashchange', openFromLocation)
          return () => {
            disposed = true
            window.removeEventListener('hashchange', openFromLocation)
          }
        }, 'approval-push: session deep link');
        ctx.slots.inject('settings.section', () => ctx.slots.register({
          name: 'settings.section',
          id: 'approval-push',
          order: 31,
          label: () => ctx.locale.bind(NS)('title'),
          locale: NS,
        }, ApprovalPushSection));
      },
    };
  },
});