/**
 * 「搜到就存」：搜索结果直接落到自己的 115 里。
 *
 * 两条路：
 *   - TG 帖子里认出了 115 分享（share_code）→ POST /api/115/share 转存，不跳网页；
 *   - 种子站结果有磁力 / .torrent 直链 → POST /api/offline/add 交给 115 离线下载。
 *
 * 落点目录都存在设置项 res_save_cid / res_save_path 里（资源搜索页可改），
 * 两个入口（资源搜索页、海报墙详情）共用同一份 —— 用户在哪存都落在同一个地方。
 */
import { ref } from 'vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import type { ResItem } from '@/composables/useResSearch'

interface SaveShareResp {
  ok: boolean
  title?: string
  files: number
  already?: boolean
  cid: string
  account_id: number
}

interface OfflineAddResp {
  ok: boolean
  hashes: string[]
  count: number
}

export function use115Share() {
  /** 正在处理的那一条（source_key|title），为空表示都空闲 */
  const busyKey = ref('')
  const cid = ref('0')
  const savePath = ref('')
  let loaded = false

  async function loadTarget(force = false) {
    if (loaded && !force) return
    try {
      const s: any = await api.config.settings()
      cid.value = s?.res_save_cid || '0'
      savePath.value = s?.res_save_path || ''
      loaded = true
    } catch {
      // 读不到设置就按根目录走：存不存得进跟这个无关
    }
  }

  async function setTarget(nextCid: string, nextPath: string) {
    cid.value = nextCid || '0'
    savePath.value = nextPath || ''
    try {
      await api.config.saveSettings({ res_save_cid: cid.value, res_save_path: savePath.value })
      toast.success('保存目录已更新', targetLabel())
    } catch (e: any) {
      toast.warning('目录没保存下来', e?.message)
    }
  }

  function targetLabel() {
    return savePath.value || '115 根目录'
  }

  const keyOf = (it: ResItem) => it.source_key + '|' + it.title
  const isBusy = (it: ResItem) => busyKey.value === keyOf(it)

  /** 能交给 115 离线下载的链接：磁力优先，其次 .torrent 直链 */
  function offlineURI(it: ResItem): string {
    if (it.magnet) return it.magnet
    if (it.torrent) return it.torrent
    return ''
  }

  async function save(it: ResItem) {
    if (!it.share_code) {
      toast.warning('这条没认出 115 分享', '点「打开」看原帖，或手动复制链接')
      return
    }
    if (busyKey.value) return
    busyKey.value = keyOf(it)
    try {
      const r = (await api.storage.saveShare({
        share_code: it.share_code,
        password: it.share_pwd,
        url: it.share_url,
        cid: cid.value,
      })) as SaveShareResp
      const name = r?.title || it.title
      if (r?.already) {
        toast.info('之前已经转存过', `${name} · 去「${targetLabel()}」里找`)
      } else {
        toast.success('已存到 115', `${name} · ${r?.files ?? 0} 个条目 → ${targetLabel()}`)
      }
    } catch (e: any) {
      toast.warning('转存失败', e?.message || '115 没给原因')
    } finally {
      busyKey.value = ''
    }
  }

  async function offline(it: ResItem) {
    const uri = offlineURI(it)
    if (!uri) {
      toast.warning('这条没有可离线的链接')
      return
    }
    if (busyKey.value) return
    busyKey.value = keyOf(it)
    try {
      const r = (await api.transfer.offlineAdd({
        uris: [uri],
        save_cid: cid.value,
        label: it.title.slice(0, 80),
      })) as OfflineAddResp
      toast.success('已提交 115 离线下载', `${it.title} · 下完会出现在「${targetLabel()}」`)
      if (!r?.count) toast.info('115 没回 info_hash', '去 115 离线列表里确认一下')
    } catch (e: any) {
      toast.warning('离线提交失败', e?.message || '115 没给原因')
    } finally {
      busyKey.value = ''
    }
  }

  return { busyKey, cid, savePath, loadTarget, setTarget, targetLabel, isBusy, offlineURI, save, offline }
}
