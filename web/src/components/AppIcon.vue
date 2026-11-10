<script setup lang="ts">
/**
 * 单色线性图标。统一 24×24、1.7 描边，避免图标库带来的体积与风格漂移。
 */
const props = withDefaults(defineProps<{ name: string; size?: number | string }>(), {
  size: 18,
})

const paths: Record<string, string> = {
  /* 导航 */
  dashboard: 'M4 4.5h6.5v6H4zM13.5 4.5H20v3.5h-6.5zM13.5 11H20v8.5h-6.5zM4 14h6.5v5.5H4z',
  accounts: 'M12 11.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7zM4.5 20c0-3.2 3.4-5.5 7.5-5.5s7.5 2.3 7.5 5.5',
  download: 'M12 3.5v10.5M7.8 10.2 12 14.4l4.2-4.2M4.5 19.5h15',
  sync: 'M4 12a8 8 0 0 1 13.4-5.9L20 8.5M20 12a8 8 0 0 1-13.4 5.9L4 15.5M20 4v4.5h-4.5M4 20v-4.5h4.5',
  share: 'M8 12a2.2 2.2 0 1 1-4.4 0 2.2 2.2 0 0 1 4.4 0zM20.4 6a2.2 2.2 0 1 1-4.4 0 2.2 2.2 0 0 1 4.4 0zM20.4 18a2.2 2.2 0 1 1-4.4 0 2.2 2.2 0 0 1 4.4 0zM7.6 10.8l8.4-3.6M7.6 13.2l8.4 3.6',
  organize: 'M4 6h16M4 11.5h9M4 17h6M16 15.5l2.5 2.5L22 14.5',
  scrape: 'M4 5.5h16v13H4zM4 9.5h16M4 15h16M8.5 5.5v13M15.5 5.5v13',
  files: 'M3.5 6.8h6l2 2h9v10.4H3.5z',
  local: 'M4 4.5h7v9H4zM13 4.5h7v5.5h-7zM13 13h7v6.5h-7zM4 16h7v3.5H4z',
  tasks: 'M5 4.5h14v3H5zM5 12h14M5 18.5h9M3.2 6h.01M3.2 12h.01M3.2 18.5h.01',
  settings:
    'M12 15.2a3.2 3.2 0 1 0 0-6.4 3.2 3.2 0 0 0 0 6.4zM12 3v2.2M12 18.8V21M4.2 7.5l1.9 1.1M17.9 15.4l1.9 1.1M4.2 16.5l1.9-1.1M17.9 8.6l1.9-1.1',
  message: 'M4 5h16v10.5H9.5L4 19.5z',
  plugin: 'M9 3v4M15 3v4M6.5 7h11v4.5a5.5 5.5 0 0 1-11 0zM12 17v4',
  logs: 'M5 4h14v16H5zM8.5 8.5h7M8.5 12h7M8.5 15.5h4.5',
  calendar: 'M4 5.5h16v14.5H4zM4 9.5h16M8 3v4M16 3v4M8 13h3v3H8z',

  /* 通用动作 */
  search: 'M10.8 17.6a6.8 6.8 0 1 0 0-13.6 6.8 6.8 0 0 0 0 13.6zM15.8 15.8 20.5 20.5',
  plus: 'M12 5v14M5 12h14',
  minus: 'M5 12h14',
  close: 'M6 6l12 12M18 6 6 18',
  check: 'M5 12.5l4.5 4.5L19 7',
  refresh: 'M20 12a8 8 0 1 1-2.3-5.7M20 4.5V9h-4.5',
  trash: 'M4.5 7h15M9.5 7V4.5h5V7M6.5 7l1 12.5h9L17.5 7M10.5 11v5M13.5 11v5',
  edit: 'M4.5 19.5h4L20 8a2.1 2.1 0 0 0-3-3L5.5 16.5zM14.5 6.5l3 3',
  more: 'M6 12h.01M12 12h.01M18 12h.01',
  filter: 'M4 6h16M7 12h10M10 18h4',
  grid: 'M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM13 13h7v7h-7z',
  list: 'M4 6h16M4 12h16M4 18h16',
  play: 'M7 4.5 19 12 7 19.5z',
  external: 'M14 4.5h5.5V10M19.5 4.5 11 13M17 14v5.5H4.5V7H10',
  info: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 11v5.5M12 7.6h.01',
  alert: 'M12 4 2.8 20h18.4zM12 10v4.5M12 17.4h.01',
  chevronLeft: 'M14.5 5.5 8 12l6.5 6.5',
  chevronRight: 'M9.5 5.5 16 12l-6.5 6.5',
  chevronDown: 'M5.5 9.5 12 16l6.5-6.5',
  arrowUp: 'M12 19V5M6 11l6-6 6 6',
  sun: 'M12 16.5a4.5 4.5 0 1 0 0-9 4.5 4.5 0 0 0 0 9zM12 2.5v2M12 19.5v2M4.2 12h2M17.8 12h2M6.3 6.3l1.4 1.4M16.3 16.3l1.4 1.4M6.3 17.7l1.4-1.4M16.3 7.7l1.4-1.4',
  moon: 'M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z',
  bell: 'M6.5 10a5.5 5.5 0 0 1 11 0c0 4 1.5 5.5 1.5 5.5H5s1.5-1.5 1.5-5.5zM10.5 19a1.8 1.8 0 0 0 3 0',
  user: 'M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM4.5 20.5c0-3.6 3.4-6 7.5-6s7.5 2.4 7.5 6',
  logout: 'M15 12H4.5M8 8l-3.5 4 3.5 4M14 4.5h5.5v15H14',
  folder: 'M3.5 6.8h6l2 2h9v10.4H3.5z',
  cloud: 'M7 18h9.6a3.4 3.4 0 0 0 .3-6.8A5.6 5.6 0 0 0 6.1 11 3.5 3.5 0 0 0 7 18z',
  file: 'M6 3.5h7l5 5v12H6zM13 3.5V9h5',
  link: 'M9.5 14.5 14.5 9.5M8 11 5.5 13.5a3.5 3.5 0 0 0 5 5L13 16M11 8l2.5-2.5a3.5 3.5 0 0 1 5 5L16 13',
  sparkles: 'M12 3.5l1.6 4.4L18 9.5l-4.4 1.6L12 15.5l-1.6-4.4L6 9.5l4.4-1.6zM18.5 15l.8 2.2 2.2.8-2.2.8-.8 2.2-.8-2.2-2.2-.8 2.2-.8z',
  image: 'M4 5h16v14H4zM4 15l4.5-4.5 4 4 3-3L20 16M9 9.5h.01',
  server: 'M4 4.5h16v6H4zM4 13.5h16v6H4zM7 7.5h.01M7 16.5h.01',
  clock: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7.5V12l3 2',
  shield: 'M12 3.5 19 6v6c0 4.4-3 7.4-7 8.5-4-1.1-7-4.1-7-8.5V6z',
  key: 'M15 9.5a3.5 3.5 0 1 1-7 0 3.5 3.5 0 0 1 7 0zM09.5 12.5 4 18v2.5h2.5V19H9v-2h2l1.5-1.5',
  history: 'M4 12a8 8 0 1 0 8-8 8 8 0 0 0-6.5 3.3M4 4v4.5h4.5M12 8v4.5l3 1.8',
  power: 'M12 3.5v8M7 6a7 7 0 1 0 10 0',
  bookmark: 'M6.5 4h11v16.5L12 16.8l-5.5 3.7z',
  fire: 'M12 3.5c3 3.5 5.5 5.6 5.5 9.2A5.5 5.5 0 0 1 12 18.2a5.5 5.5 0 0 1-5.5-5.5c0-1.6.6-2.8 1.6-4 .3 1.4 1 2.2 2 2.6-.4-2.6.3-5 1.9-7.8z',
}

const d = (n: string) => paths[n] ?? paths.info
</script>

<template>
  <svg
    :width="props.size"
    :height="props.size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.7"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <path :d="d(props.name)" />
  </svg>
</template>
