package store

import "qinyuan/internal/graphstore"

// 编译期断言：真实存储必须满足 P2-4 的图访问契约。
//
// 放在实现方（而不是契约包）是刻意的：graphstore 不 import store，
// 于是"契约 → 领域类型"、"实现 → 契约"两条依赖都是单向的。
// 任何人改了 store 的签名，这里会先炸，而不是在 agent 的调用点。
var _ graphstore.Graph = (*Store)(nil)
