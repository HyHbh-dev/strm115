<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import { errText, login, refreshSession, runSetup, session } from '@/stores/session'
import { isDark, toggleTheme } from '@/stores/ui'
import { toast } from '@/stores/toast'

const route = useRoute()
const router = useRouter()

const mode = ref<'login' | 'setup'>('login')
const username = ref('')
const password = ref('')
const confirm = ref('')
const otp = ref('')
const needOtp = ref(false)
const loading = ref(false)
const error = ref('')

const isSetup = computed(() => mode.value === 'setup')

function finish() {
  const redirect = (route.query.redirect as string) || '/'
  router.replace(redirect)
}

async function submit() {
  error.value = ''
  if (!username.value.trim() || !password.value) {
    error.value = '请输入账号和密码'
    return
  }
  if (isSetup.value && password.value !== confirm.value) {
    error.value = '两次输入的密码不一致'
    return
  }
  if (password.value.length < 6) {
    error.value = '密码至少 6 位'
    return
  }

  loading.value = true
  try {
    if (isSetup.value) {
      await runSetup(username.value.trim(), password.value)
      toast.success('管理员账号已创建')
    } else {
      await login(username.value.trim(), password.value, needOtp.value ? otp.value : undefined)
    }
    toast.success('欢迎回来')
    finish()
  } catch (e) {
    const msg = errText(e, '登录失败')
    // 需要二次验证时后端会回 otp，这里把验证码输入框放出来
    if (/otp|验证码/i.test(msg)) needOtp.value = true
    error.value = msg
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  await refreshSession()
  if (session.setupNeeded || route.query.setup === '1') {
    mode.value = 'setup'
  }
  if (session.authed) finish()
})
</script>

<template>
  <div class="login">
    <div class="hero">
      <div class="hero-brand">
        <svg viewBox="0 0 32 32" width="34" height="34" aria-hidden="true">
          <defs>
            <linearGradient id="lg" x1="0" y1="0" x2="1" y2="1">
              <stop stop-color="#ff9d4d" />
              <stop offset="1" stop-color="#d9480f" />
            </linearGradient>
          </defs>
          <rect width="32" height="32" rx="8.5" fill="url(#lg)" />
          <g stroke="#fff" stroke-linecap="round" stroke-width="2.2">
            <path d="M6.5 11.5h4" opacity=".55" />
            <path d="M4.8 16h6.2" opacity=".85" />
            <path d="M6.5 20.5h4" opacity=".55" />
            <path d="M16 11.5h10" opacity=".75" />
            <path d="M16 16h7" opacity=".55" />
            <path d="M16 20.5h10" opacity=".35" />
          </g>
        </svg>
        <strong>StrmStation</strong>
      </div>

      <h1>115 网盘 → STRM → Emby<br />整理、刮削、订阅，一条链路跑完</h1>
      <p class="hero-sub">
        同步目录生成 STRM，播放 302 直连 CDN；片名识别、海报 NFO、洗版策略、任务台账都在这里。
      </p>

      <ul class="hero-list">
        <li><AppIcon name="sync" :size="16" />全量 / 增量 / 深度删除，改地址不用重扫</li>
        <li><AppIcon name="organize" :size="16" />识别疑难片名，同片多版本按洗版策略处理</li>
        <li><AppIcon name="local" :size="16" />海报墙按平台、类型、大小筛，点开即搜资源</li>
      </ul>

      <div class="hero-foot mono">v{{ session.version || '0.7.1' }}</div>
    </div>

    <div class="pane">
      <button class="btn btn-icon theme" :title="isDark() ? '浅色' : '深色'" @click="toggleTheme">
        <AppIcon :name="isDark() ? 'sun' : 'moon'" />
      </button>

      <form class="card glass" @submit.prevent="submit">
        <div class="card-head">
          <h2>{{ isSetup ? '创建管理员账号' : '登录' }}</h2>
          <p class="muted small">
            {{
              isSetup
                ? '第一次打开需要设置管理员账号，之后用它登录。'
                : '用管理员账号登录，会话 7 天内有效。'
            }}
          </p>
        </div>

        <label class="lb">账号</label>
        <input
          v-model="username"
          class="input"
          :placeholder="isSetup ? '如 admin' : '用户名 / 邮箱'"
          autocomplete="username"
          autofocus
        />

        <label class="lb">密码</label>
        <input
          v-model="password"
          class="input"
          type="password"
          :placeholder="isSetup ? '至少 6 位' : '请输入密码'"
          :autocomplete="isSetup ? 'new-password' : 'current-password'"
        />

        <template v-if="isSetup">
          <label class="lb">确认密码</label>
          <input v-model="confirm" class="input" type="password" placeholder="再输一次" />
        </template>

        <template v-if="needOtp">
          <label class="lb">两步验证码</label>
          <input v-model="otp" class="input mono" placeholder="6 位数字" inputmode="numeric" maxlength="6" />
        </template>

        <p v-if="error" class="err">{{ error }}</p>

        <button class="btn btn-primary btn-lg btn-block" :disabled="loading" type="submit">
          <AppIcon v-if="loading" name="refresh" :size="16" />
          {{ loading ? '处理中…' : isSetup ? '创建并进入' : '登录' }}
        </button>

        <p class="hint muted small">
          忘记密码：停掉容器后删除 <code>data/*.db</code> 里的凭据表，重启即可重新设置。
          <span v-if="session.error" style="color: var(--warning)">
            当前无法连接服务：{{ session.error }}
          </span>
        </p>
      </form>
    </div>
  </div>
</template>

<style scoped>
.login {
  display: grid;
  grid-template-columns: minmax(0, 1.05fr) minmax(420px, 0.95fr);
  min-height: 100dvh;
}
.hero {
  position: relative;
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 18px;
  padding: clamp(28px, 6vw, 72px);
  background:
    radial-gradient(70% 60% at 20% 15%, color-mix(in oklab, var(--accent) 22%, transparent), transparent 70%),
    radial-gradient(60% 50% at 90% 90%, color-mix(in oklab, var(--info) 18%, transparent), transparent 70%),
    var(--surface);
  border-right: 1px solid var(--border);
  overflow: hidden;
}
.hero-brand {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 15px;
  letter-spacing: -0.01em;
}
.hero h1 {
  font-size: clamp(24px, 3vw, 36px);
  line-height: 1.3;
  max-width: 15em;
}
.hero-sub {
  max-width: 30em;
  color: var(--muted);
  font-size: 13.5px;
  line-height: 1.75;
}
.hero-list {
  display: flex;
  flex-direction: column;
  gap: 9px;
  margin: 8px 0 0;
  padding: 0;
  list-style: none;
  font-size: 13px;
  color: color-mix(in oklab, var(--foreground) 78%, transparent);
}
.hero-list li {
  display: flex;
  align-items: center;
  gap: 9px;
}
.hero-list svg {
  color: var(--accent);
}
.hero-foot {
  position: absolute;
  left: clamp(28px, 6vw, 72px);
  bottom: 26px;
  color: var(--muted);
  font-size: 11.5px;
}

.pane {
  position: relative;
  display: grid;
  place-items: center;
  padding: 28px;
}
.theme {
  position: absolute;
  top: 18px;
  right: 18px;
}
.card {
  width: min(380px, 100%);
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 26px 24px 20px;
  border-radius: var(--r-xl);
  background: var(--glass-bg);
}
.card-head {
  margin-bottom: 6px;
}
.card-head h2 {
  font-size: 19px;
  margin-bottom: 4px;
}
.lb {
  margin-top: 6px;
  font-size: 12.5px;
  font-weight: 600;
}
.card .btn-lg {
  margin-top: 14px;
}
.err {
  margin-top: 4px;
  color: var(--danger);
  font-size: 12.5px;
}
.hint {
  margin-top: 10px;
  line-height: 1.6;
}
.hint code {
  font-family: var(--font-mono);
  font-size: 11.5px;
  padding: 1px 4px;
  border-radius: 4px;
  background: var(--surface-secondary);
}
@media (max-width: 860px) {
  .login {
    grid-template-columns: minmax(0, 1fr);
  }
  .hero {
    display: none;
  }
}
</style>
