<script setup lang="ts">
/**
 * 资源搜索独立页 —— 左边结果、右边详情。
 *
 * 以前点结果看详情是整页跳到 /title，回来时搜索页靠 KeepAlive 复原；实际用
 * 下来只要有一次缓存没命中（刷新、直接打开链接、换设备），回来就是一屏空白，
 * 得重新搜一遍。现在详情直接开在右栏：
 *   - 左栏的搜索词、候选、整屏结果一直在，关掉详情就回到原样；
 *   - 点结果行 / 认片候选都只是「选中」，不产生导航；
 *   - 「用这部片搜资源」把片名填回左侧重搜，不用自己再敲一遍。
 *
 * 用 runId 而不是 watch(q)：同一个词连按两次「搜索」也要能重新打一次网络。
 */
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import ResourceSearch from '@/components/ResourceSearch.vue'
import TitleDetail, { type TitleTarget } from '@/components/TitleDetail.vue'

// KeepAlive 按组件名匹配，名字必须和 AppShell 里的 include 一致。
defineOptions({ name: 'SearchPage' })

const route = useRoute()
const q = ref('')
const en = ref('')
const runId = ref(0)

/** 右栏正在看的片；null 表示没打开详情，只看结果列表。 */
const detail = ref<TitleTarget | null>(null)

// 记住上一次真正搜过的词：从详情页/外链回到 /search?q=同一个词时，
// 如果每次都 runId++，KeepAlive 就白保了 —— 结果还在，却又重新打一次网络。
let lastQ = ''
let lastEn = ''

function syncFromRoute() {
  const nq = String(route.query.q || '').trim()
  const ne = String(route.query.en || '').trim()
  if (!nq) return
  if (nq === lastQ && ne === lastEn) return
  lastQ = nq
  lastEn = ne
  q.value = nq
  en.value = ne
  runId.value++
}

// immediate：直接带 ?q= 打开链接时，一进页面就开始搜。
watch(() => route.fullPath, syncFromRoute, { immediate: true })

function openDetail(t: TitleTarget) {
  detail.value = { ...t }
}

function closeDetail() {
  detail.value = null
}

/** 面板里的「用这部片搜资源」：填回左侧并重搜，同时收起面板看结果。 */
function findWithTitle(p: { q: string; en: string }) {
  if (!p.q) return
  q.value = p.q
  en.value = p.en || ''
  runId.value++
  detail.value = null
}

const hasDetail = computed(() => !!detail.value)
</script>

<template>
  <div class="split" :class="{ 'has-detail': hasDetail }">
    <div class="split-main">
      <ResourceSearch
        :initial-q="q"
        :initial-en="en"
        :run-id="runId"
        @open-detail="openDetail"
      />
    </div>

    <aside v-if="hasDetail" class="split-side">
      <div class="row-between side-head">
        <button class="btn btn-sm btn-tertiary" @click="closeDetail">
          <AppIcon name="chevronLeft" :size="14" />返回搜索结果
        </button>
        <button class="btn btn-sm btn-ghost" title="关闭详情" @click="closeDetail">
          <AppIcon name="close" :size="14" />
        </button>
      </div>
      <TitleDetail v-if="detail" :target="detail" compact @find="findWithTitle" />
    </aside>
  </div>
</template>

<style scoped>
.split {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: 16px;
  align-items: start;
}
/* 宽屏才并排：结果至少要有 640px 才放得下一行标题 + 按钮组。 */
.split.has-detail {
  grid-template-columns: minmax(0, 1fr) minmax(360px, 440px);
}
.split-side {
  position: sticky;
  top: 78px;
  max-height: calc(100vh - 96px);
  overflow: auto;
  padding-bottom: 8px;
}
.side-head {
  gap: 8px;
  margin-bottom: 10px;
}
/* 窄屏（含 iPad 竖屏、手机）：详情改成整屏覆盖，带自己的返回按钮。
   和页面导航权限一致 —— 关掉就回到列表，不产生一次路由跳转。 */
@media (max-width: 1180px) {
  .split.has-detail {
    grid-template-columns: minmax(0, 1fr);
  }
  .split-side {
    position: fixed;
    inset: 0;
    z-index: 60;
    max-height: none;
    overflow: auto;
    padding: 12px 12px calc(16px + env(safe-area-inset-bottom));
    background: var(--bg-base);
  }
}
</style>
