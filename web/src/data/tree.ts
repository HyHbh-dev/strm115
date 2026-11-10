/** 网盘目录树的节点。trail 是从根到这个节点的完整路径，用来拼面包屑。 */
export interface DirNode {
  id: string
  name: string
  trail: { id: string; name: string }[]
  kids?: DirNode[]
  open?: boolean
  loaded?: boolean
  loading?: boolean
}
