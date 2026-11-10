<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppShell from '@/layout/AppShell.vue'
import MediaDetailDrawer from '@/components/MediaDetailDrawer.vue'
import { authBus } from '@/api/client'
import { refreshSession, session } from '@/stores/session'
import { toast } from '@/stores/toast'

const route = useRoute()
const router = useRouter()

const bare = computed(() => !!route.meta.public)

onMounted(async () => {
  authBus.addEventListener('expired', async () => {
    toast.warning('登录已过期，请重新登录')
    session.authed = false
    await router.replace({ name: 'login' })
  })
  if (!session.ready) await refreshSession()
})
</script>

<template>
  <AppShell v-if="!bare" />
  <RouterView v-else />
  <!-- 全站共用的影视详情抽屉：任何页面 openDetail() 都开这一个 -->
  <MediaDetailDrawer v-if="!bare" />
</template>
