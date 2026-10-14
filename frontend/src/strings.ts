// All user-facing strings live here so the T7 i18n pass is a swap of this map.
// Values are zh-CN source copy (the approved prototype language).

export const S = {
  appName: "Auto Resume",

  nav: { dashboard: "仪表盘", settings: "设置", about: "关于" },
  sidebarFoot: { running: "守护进程运行中", stopped: "守护进程未运行" },

  strip: {
    running: "运行中",
    paused: "已暂停",
    offline: "未运行",
    meta: (roots: number, transport: string, scan: string) =>
      `监控 ${roots} 个位置 · ${transport} · 扫描 ${scan}`,
    refresh: "刷新",
    pause: "暂停",
    resume: "恢复",
  },

  stats: {
    quotaWait: "配额等待",
    pending: "等待中",
    running: "执行中",
    attention: "需关注",
    stopped: "已停止",
  },

  queue: {
    empty: "队列为空 — 没有需要重试的任务",
    tags: {
      pending: "等待中",
      starting: "启动中",
      running: "执行中",
      waiting_for_reset: "配额等待",
      reset_due: "配额已到",
      needs_attention: "需关注",
      stopped: "已停止",
    },
    sub: {
      untilRetry: "后重试",
      untilResume: "后恢复",
      dueNow: "可恢复",
      running: "执行中",
      starting: "启动中",
      attention: "需处理",
      stopped: "已停止",
    },
    btn: {
      retryNow: "立即重试",
      cancel: "取消",
      restart: "重新开始",
      resume: "恢复",
    },
    writesPending: "写操作将在后续版本接入",
  },

  class: {
    none: "—",
    transient: "瞬时故障",
    rate_limit: "请求限流",
    server: "供应商故障",
    auth_transient: "登录抖动",
    auth_limited: "登录异常",
    empty_response: "空响应",
    unknown: "未知故障",
  } as Record<string, string>,

  action: {
    dispatching: "派发中",
    goal_resume: "目标恢复",
    goal_active: "目标活跃",
    conversation_continue: "普通续聊",
    subagent_continue: "子代理续聊",
    goal_block: "目标阻塞",
  } as Record<string, string>,

  meta: {
    recovery: (attempt: number, max: number | undefined, consec: number, maxConsec: number | undefined) => {
      const parts: string[] = [];
      if (max) parts.push(`恢复 ${attempt}/${max}`);
      if (maxConsec) parts.push(`无进展 ${consec}/${maxConsec}`);
      return parts.join(" · ");
    },
    bindingReset: (when: string) => `绑定窗口 ${when} 重置`,
    stoppedReason: (reason: string) => `停止原因 ${reason}`,
    stopReasons: {
      auth_attempt_limit: "登录尝试达到上限",
      recovery_attempt_limit: "恢复尝试达到上限",
      consecutive_retry_limit: "连续无进展达到上限",
      manual: "手动停止",
    } as Record<string, string>,
  },

  quota: {
    title: "配额剩余",
    window: "配额窗口",
    binding: "绑定中",
    idle: "未绑定",
    left: (pct: number) => `剩 ${pct}%`,
    resetsAt: (when: string) => `${when} 重置`,
    noReset: "重置时间未知",
    waiting: (n: number) => `${n} 个任务排队等待`,
    plenty: "余量充足",
    empty: "暂无配额数据",
  },

  daemon: {
    title: "守护进程",
    live: "活跃",
    memory: (mb: number) => `内存 ${mb.toFixed(1)} MB`,
    lastScan: (ago: string) => `上次扫描 ${ago}`,
    justNow: "刚刚",
    secondsAgo: (n: number) => `${n} 秒前`,
    pidLimit: "PID / 上限",
    uptime: "运行时长",
    autoStart: "开机自启",
    autoStartOn: "已启用",
    autoStartOff: "未启用",
    transport: "传输",
    restart: "重启守护进程",
    restartConfirm: "重启期间约 5 秒无自动恢复，确认？",
    yes: "确认",
    no: "取消",
    restartPending: "重启指令将在后续版本接入",
    guard: "内存保护已触发",
  },

  settings: {
    title: "设置",
    autoResume: "自动恢复",
    autoResumeDesc: "暂停后仅监控",
    shared: "共享后台",
    sharedDesc: (port: number) => `端口 ${port}`,
    notify: "系统通知",
    notifyDesc: "撞墙/停止推送",
    boot: "开机自启",
    bootDesc: "登录即启动",
    lang: "语言 / Language",
    langDesc: "界面显示语言",
    tz: "时区显示",
    tzDesc: "所有时间按本地时区显示",
    tzSystem: "跟随系统",
    retryPolicy: "重试策略",
    prompt: "重试文字",
    recMax: "恢复上限",
    noMax: "无进展上限",
    authMax: "登录重试上限",
    delay: "等待策略",
    initDelay: "首次等待（秒）",
    maxDelay: "最长等待（秒）",
    delayStep: "等待递增（秒）",
    memLimit: "内存上限（MB）",
    save: "保存",
    reset: "重置",
    pendingNote: "设置写入将在后续版本接入（当前仅展示）",
    strategies: { exponential: "指数退避", fixed: "固定", linear: "线性" } as Record<string, string>,
  },

  about: {
    title: "关于",
    version: "版本",
    daemon: "守护进程",
    daemonPid: "守护进程 PID",
    dataDir: "数据目录",
    log: "日志",
    openLog: "打开日志目录",
    copyDiag: "复制诊断信息",
    copied: "已复制",
    unknown: "—",
  },

  offline: {
    banner: "无法连接守护进程 — 显示最近一次数据",
    hint: "通过 ?api=<地址>&token=<令牌> 连接，或 ?preview=1 查看演示",
    connecting: "未配置连接参数",
  },

  time: {
    today: "今天",
    tomorrow: "明天",
  },
};
