<script setup lang="ts">
/**
 * 影视库：海报墙 + 刮削与字幕，一页装完。
 *
 * 合并的原因：这两件事本来就是同一个循环——看墙上缺什么 → 去刮一次 →
 * 刮完墙上就有海报了。以前拆成 /scrape 和 /local 两个页面，用户得在两个
 * 标签之间来回切，还常常忘了刮完要回墙面刷新。
 *
 * 「发现片单」（TMDB 榜单）已经从这一页移走：找片属于「发现与订阅」那条线，
 * 挂在影视库里既让本页变重，也变成两处榜单各自刷一遍。要看榜单去
 * /media-transfer，那边有完整的筛选、订阅与转存。
 *
 * 标签记在 URL 的 ?tab= 里，刷新不丢；/scrape 与 /local 两个旧地址由路由
 * 重定向过来，老书签照样能用。
 */
import { ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import HTabs from '@/components/HTabs.vue'
import AppIcon from '@/components/AppIcon.vue'
import LibraryWall from '@/components/LibraryWall.vue'
import ScrapeConsole from '@/components/ScrapeConsole.vue'

const route = useRoute()
const router = useRouter()

const TABS = [
  { value: 'wall', label: '海报墙' },
  { value: 'scrape', label: '刮削与字幕' },
] as const

function initialTab(): string {
  const t = String(route.query.tab ?? '')
  return TABS.some((x) => x.value === t) ? t : 'wall'
}

// 用 string 而不是联合类型：HTabs 的 v-model 是 string，
// 收窄成联合类型要在模板里到处断言，不划算。
const tab = ref<string>(initialTab())

function goNewLibrary() {
  void router.push('/accounts?tab=library&new=1')
}

// 用 replace 而不是 push：切标签是页内视图变化，不该在浏览器历史里留一串记录，
// 点「后退」应该退回上一个页面，而不是退回上一个标签。
watch(tab, (v) => {
  if (String(route.query.tab ?? '') === v) return
  void router.replace({ path: '/library', query: v === 'wall' ? {} : { tab: v } })
})

// 同一路由内手改地址栏（比如点了侧栏）时，跟着走。
watch(
  () => route.query.tab,
  () => {
    const v = initialTab()
    if (v !== tab.value) tab.value = v
  },
)
</script>

<template>
  <div class="col" style="gap: 16px">
    <div class="row wrap" style="gap: 10px; justify-content: space-between">
      <HTabs v-model="tab" :items="TABS.map((t) => ({ ...t }))" />
      <!-- 海报墙只展示已入库的片；新建媒体库（115 / 夸克TV）在「账号与媒体库」里，
           这里给个直达入口，免得在影视库页找不到扫描选项。 -->
      <button class="btn btn-sm btn-tertiary new-lib-btn" @click="goNewLibrary">
        <AppIcon name="plus" :size="14" />新建媒体库（115 / 夸克TV）
      </button>
    </div>

    <!-- 两个标签都保持「单独打开也能用」：各自有自己的库选择与筛选，
         每次切换重新挂载会重新拉一遍数据，刮削完切回来就是新的。 -->
    <LibraryWall v-if="tab === 'wall'" @go-scrape="tab = 'scrape'" />
    <ScrapeConsole v-else @go-wall="tab = 'wall'" />
  </div>
</template>

<style scoped>
/* 窄屏：顶部行已有 wrap，按钮文字允许折行，别把「新建媒体库」挤成一团 */
@media (max-width: 900px) {
  .new-lib-btn {
    white-space: normal;
    text-align: left;
  }
}
</style>
