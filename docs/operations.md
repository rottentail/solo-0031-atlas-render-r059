# 运行与恢复

生产启动前构建 `go build -o atlas ./cmd/atlas`。通过ATLAS_STATE指定独立可写状态路径，通过ATLAS_ADDR指定受信环境的监听地址。状态目录应由运行进程拥有；临时写文件与最终文件位于同一个目录。

同一状态路径的第二个进程会因为独占文件锁拒绝启动。锁文件可长期存在，锁的有效性由操作系统进程锁决定。不要通过删除锁文件启动第二个写进程。

SIGTERM或中断信号会停止领取任务，取消执行中的计算，等待HTTP请求和执行循环结束。执行中的任务保留running，下一次启动会重新入队。SIGKILL恢复用 `python3 scripts/walkthrough.py --workflow recover-render` 演练；只会结束该脚本自己启动的实例。

备份时先正常停止实例，复制状态JSON；恢复时向空的独立状态路径复制，再启动。不要把运行中手工编辑过的JSON直接作为新事实。启动校验拒绝不支持版本、损坏摘要、孤立关联和非法文档。备份包含PNG字节与全部快照引用闭包，无需从外部素材服务恢复。

容量观察入口是GET /v1/status。它报告未压缩的持久结果PNG字节总量、存活与已清理结果数量和保留预览计数，不代表堆内存峰值。完整状态在事务中复制并编码，因此长期积累会增加提交成本。当前未提供任务或快照删除；部署应按预期数据量规划状态盘和实例周期。历史事件超过500条会截断，但sequence继续增加。

## 结果保留与清理

清理是“先预览、再执行”的两步流程，入口与旧的单条 DELETE 共用同一套规则：

1. 预览：`POST /v1/retention/previews`，提交 `artifactIds`（精确选择）或 `before`（选择该时间前完成的存活结果），可选 `note`。响应列出每个结果的 `state`、`jobState`、`digest`、尺寸、字节数、`eligible`、`reason` 和 `hold`，并汇总 `eligibleCount`、`bytesReclaimable`。预览只读取，不删除。
2. 执行：`POST /v1/retention/previews/{id}/execute`，提交预览返回的 `token` 与可选 `artifactId`（只清理该预览覆盖并判定可清理的一条）。省略 `artifactId` 时清理全部可清理项。

预览是**短时观察，不是长期授权**。默认10分钟内可执行，可用 `ATLAS_RETENTION_PREVIEW_TTL` 缩短或延长（1s–24h）。执行时检验：

- `token` 必须与预览计划完全一致，手工改动或伪造返回409 `token-mismatch`。
- 预览过期返回409 `expired`；重启后返回409 `restarted`；同一预览已执行返回409 `already-executed`。
- 预览之后有任何结果完成（新任务成功、结果字节或摘要变化），结果集指纹改变，返回409 `results-changed`。
- 预览之后有并发下载或像素比较读取了预览中的结果，返回409 `access-changed`，`artifactIds` 指明被读取的结果。
- 执行时该结果仍被下载或比较占用，返回409 `artifact-in-use`；服务端先租约、再原子提交，读取中的响应不会被截断。
- 预览因过期、重启、结果集或读取变化被拒绝后会被标记为 `stale`，不能重放，也不占用待执行预览上限。

对同一预览以相同 `token` 重复执行是幂等重放：返回 `replayed: true` 和首次结果，不重复删除。单条兼容入口 `DELETE /v1/artifacts/{id}` 内部同样先生成只覆盖该结果的预览再执行；已清理结果返回200且 `state: already-purged`，不存在的ID返回404。

清理后 `GET /v1/artifacts/{id}` 返回410，响应体仍带 `jobId`、`jobState: succeeded`、`digest`、`width`、`height`、`purged: true` 和 `purgedAt`。任务状态、结果摘要与幂等收据都不变；清理绝不把成功任务改成失败或未完成。`GET /v1/retention/previews` 列出保留预览（最新在前），`GET /v1/retention/previews/{id}` 读取单个预览及当前 `expired`、`currentlyHeld` 信号。

运维演练：`python3 scripts/walkthrough.py --workflow retention-drill` 通过实际 HTTP 进程覆盖预览、令牌篡改、读取后失效、新结果失效、重启失效、幂等重放、并发读取竞争和清理后410。到期路径用 `python3 scripts/walkthrough.py --workflow retention-expiry --preview-ttl 1s` 演练。

故障时保留当前状态文件和服务日志，在副本上检查。500表示事务未能完成；客户端可先读取原请求目标确认状态。提交任务时使用稳定幂等键，不能因为连接中断盲目换键。409应结合当前场景revision、快照state、收据语义或清理预览的 `reason` 判断：`expired`、`restarted`、`results-changed`、`access-changed` 都应重新预览后再执行，不能重试旧令牌。410说明成功结果字节已清理，任务成功事实仍保留。

`deploy/atlas.service` 是Linux systemd配置示例，需按实际用户、二进制和状态路径调整。macOS可直接运行同一二进制。当前未验证容器部署、跨进程共享写入、跨机器文件系统锁或网络存储。
