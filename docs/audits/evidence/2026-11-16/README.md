# 验收证据索引

- `review-scheduler.md`、`review-correctness.md`、`review-notify.md`：独立实现外的交叉审查，含红灯复现、固定 SHA 和修复复验。
- `review-notify-wiring.md`：另一名 Agent 的装配/API/E2E 测试代码复核。
- `*_test.go.txt`：审查者在仓库外编写的独立探针。以 txt 保存，避免被正常 Go 包构建发现；对应旧/新 SHA 与 overlay 用法见审查报告。报告中的 `/private/tmp` 是本次会话的执行路径，不要求其他环境使用相同绝对路径。
- `verify_migrations.py` 与 `migration-results.json`：最终迁移 32 的真实 ledger 验证脚本和回执。脚本创建并清理独立数据库容器，既有开发数据库不在目标范围。

修复后的长期回归已经进入相应实现 PR 的正常测试：scheduler/status 与 pools integration、incident/mapping_upgrade integration、app/fault_notification activation 和 fallback integration、E2E/fault_notify。以这些测试作为后续持续验证入口；外置探针用于追溯独立审查，而非替代项目测试。

联合构建、全仓 integration/race、E2E 与 CI 的最终结果见上级 `2026-11-16-failure-mode-acceptance.md`。
