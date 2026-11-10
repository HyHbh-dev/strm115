<script setup lang="ts">
/**
 * 影视详情页 —— 一个真正的页面，不是抽屉。
 *
 * 从「发现与订阅」的认片搜索、平台海报墙点进来，先看到海报 / 简介 /
 * 季集 / 入库与订阅状态，再由用户自己决定要不要「找资源」。
 *
 * 详情正文已经抽成 TitleDetail 组件（资源搜索页的右侧面板用的是同一份），
 * 这里只剩「按路由取参数 + 顶部返回」这层壳。
 */
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import TitleDetail from '@/components/TitleDetail.vue'
import { clearOrigin, titleOrigin } from '@/stores/navMemory'

const route = useRoute()
const router = useRouter()

const target = computed(() => ({
  type: String(route.query.type || ''),
  tmdbId: String(route.query.tmdb_id || ''),
  q: String(route.query.q || ''),
  year: String(route.query.year || ''),
}))

const titleFallback = computed(() => String(route.query.q || '').trim())

/**
 * 返回按钮的文案：跟着来路走。
 *
 * 以前写死「返回发现与订阅」，从搜索结果点进来的人看到这行字会以为
 * 要把自己扔回另一个页面；实际也确实如此 —— 那时候返回是硬编码的。
 */
const backLabel = computed(() => {
  if (titleOrigin.value) return titleOrigin.value.label
  if (window.history.state?.back) return '返回'
  return '返回搜索结果'
})

/**
 * 返回上一站。
 *
 * 优先用「来路」而不是 history.back()：资源搜索页是 KeepAlive 的，按地址回去
 * 能原样恢复搜索词和结果；history.back() 在个别客户端（内置浏览器、PWA）
 * 会把用户带出应用。
 */
function goBack() {
  const from = titleOrigin.value
  clearOrigin()
  if (from?.path) {
    void router.push(from.path)
    return
  }
  if (window.history.state?.back) {
    router.back()
    return
  }
  // 直接打开链接（分享、新标签）时没有来路：默认落到资源搜索页，而不是
  // 「发现与订阅」—— 看完详情最可能要做的事是找资源。
  void router.push({ path: '/search', query: { q: titleFallback.value } })
}

/** 找资源：带片名 + 英文原名进资源搜索页，进页面就自动开搜。 */
function onFind(p: { q: string; en: string }) {
  router.push({
    path: '/search',
    query: {
      q: p.q || titleFallback.value,
      en: p.en === p.q ? '' : p.en,
    },
  })
}
</script>

<template>
  <div class="col" style="gap: 14px">
    <div class="row-between">
      <button class="btn btn-sm btn-tertiary" @click="goBack">
        <AppIcon name="chevronLeft" :size="14" />{{ backLabel }}
      </button>
    </div>
    <TitleDetail :target="target" @find="onFind" />
  </div>
</template>
