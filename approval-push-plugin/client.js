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
          h('li', {}, t('rule.privacy'))));
    }

    return {
      inject: ['slots', 'locale'],
      apply(ctx) {
        ctx.effect(() => ctx.locale.register(NS, {
          zh: {
            'title': '审批推送到手机',
            'note': 'agent 请求审批时，已配对的手机会收到通知卡片，可直接允许一次或拒绝；点击卡片打开 App。',
            'rule.claim': '只有手机在线订阅时才接管审批；手机不在，审批照常留在桌面。',
            'rule.failClosed': '接管后无人应答＝拒绝（超时自动拒绝，沉默不是同意）。',
            'rule.privacy': '提问通知不含问题文本，只提示去 dsh 查看。',
          },
          en: {
            'title': 'Approval push to phone',
            'note': 'When an agent asks for approval, paired phones receive a notification card with allow-once / reject actions; tapping opens the App.',
            'rule.claim': 'Approvals are claimed only while a phone is subscribed; otherwise they stay on the desktop.',
            'rule.failClosed': 'A claimed ask nobody answers is rejected on timeout — silence is never consent.',
            'rule.privacy': 'Question notifications carry no question text, only a nudge to open dsh.',
          },
        }), 'approval-push: dictionaries');
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