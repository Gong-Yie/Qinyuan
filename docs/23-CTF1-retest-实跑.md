# CTF1-retest 实跑报告（run #42）

> 目标：`node4.anna.nssnode.cn:26759`（用户自建的同款靶机），材料同 `docs/19` 的附件。
> 目的：验证 M13–M16 在真实任务上的效果，并尝试拿到 flag。
> 结论：**flag 没拿到**；但 M14 把"2 分钟定位真实端点"变成了现实，M13/M15/M16 都在这次实跑里被验证。
> 环境：`serve` + 2 worker，`timeout_seconds=1200`，2026-11-11 15:35:21 → 15:55:40。

---

## 1. 数字

| 指标 | run #41（修复前） | run #42（修复后） |
|---|---|---|
| 时长 | 26m40s | **20m19s**（含收尾） |
| 意图 | 13（5 blocked） | 10（4 blocked / 2 exhausted / 4 done） |
| 工具调用 | 300 | **164** |
| 其中 HTTP | 约 230 | **52** |
| 其中归档 | 0 | **75**（archive_read 43 + archive_list 32） |
| 事实 / 发现 | 18 / 1 | 14 / 1 |
| 名义 input | 2,076,729 | **1,084,221** |
| cache_read | 1,732,224（83%） | 900,224（83%） |
| 有效 input | ≈344k | **≈184k** |
| 终态 | 人工暂停（paused） | 超时收尾（timeout） |

用一半的有效 token、四分之一的 HTTP 请求，做到了 #41 没做到的事（见 §2）。

---

## 2. 它做对了什么（时间线）

| 时间 | 事件 |
|---|---|
| 15:35:57 | 第 1 轮就派了"静态逆向附件"+"线上基线"两条意图（M14 的清单进了态势） |
| 15:36:39 | 基线事实：Jetty 9.4.49 / `/` → `/index` / `/api/*` 一律 403 |
| **15:37:17** | **fact #341：`GET /index` 与 `GET /api/test`（base64 → ObjectInputStream.readObject）是唯一两个路由，后者是 RCE 入口** |
| 15:37:02 | 读全 `hello.jsp` / `api.jsp` / `web.xml` / `mvc-dispatcher-servlet.xml` |
| 15:38:49 | 列出 `WEB-INF/lib` 12 个 jar，判断"无 CC/C3P0/Groovy/BeanUtils，ysoserial 常见链不可用" |
| 15:40:52 | 用 strings 反推 `bypassFilter` / `loginFilter` 的判定逻辑 |
| 15:41:09 | 判定 `CustomObjectInputStream.resolveClass` 是**黑名单**、未覆盖 `resolveProxyClass` |

**关键差别**：run #41 花了 26 分钟都没找到 `/api/test`（一直在打不存在的 `/api/flag`）；
这次 **2 分钟**就从 `web.xml` + `ConstantPool strings` 里读出来了。这是 M14 的直接收益。

---

## 3. 三处"这次实跑抓出来"的验证

1. **M13 的失败可见**：`get_worker_output` 带回了 `last_error`，planner 在 #329 blocked 后
   写了"上一轮 #329 因输出预算耗尽而 blocked"，并把后续意图改成"极窄单文件读"。
2. **M15 停摆重规划**：15:40:07–15:42:47 连续 4 次"探索停摆 → 立即重新规划"，
   15:43–15:48 转 2 分钟巡检 ×3，15:51:26 退回心跳 —— 全部按设计执行（旧版会直接干等 10 分钟）。
3. **M16 的来由**：4 条意图（#329/#336/#346/#347）死在
   `llm: reasoning-only: finish=length，推理 4096 token`。这正是 M13 识别、M16 修掉的坑；
   #42 跑的是旧二进制，所以它们仍然 blocked —— 下一个 run 才会用上翻倍预算。

---

## 4. 为什么还是没拿到 flag

### 4.1 差一步的旁路（agent 没跨过去）

我手工验证过（同一次会话，针对这台靶机）：

| 请求 | 结果 |
|---|---|
| `GET /api/test` | **403** `forbidden`（9 字节，loginFilter） |
| `GET //api/test` | **500 NPE**（`Base64.decode(null)`）→ **请求已经进到 ApiController** |
| `GET //api/test?data=<序列化 String>` | **200**，返回 `api test` 视图 → **反序列化真的执行了** |

也就是 `//` 前缀旁路成立：`loginFilter` 的 `/api/*` 按 servletPath 前缀匹配，`//api/...` 不匹配；
而 Spring 的 `AntPathMatcher` 忽略空段，仍然映射到 `/api/...`。

**agent 试过 `//api`（404），但 52 个 HTTP 请求里没有一次 `//api/test`** ——
它把 404 理解成"没绕过"，没有把"403 → 404-by-servlet（甚至 500）"当成穿透信号。
这正是 `docs/19 §3` 记下的同一个盲点，这次只是在更接近终点的地方又踩了一次。

### 4.2 最后一段是 Java 反序列化 gadget

即使绕过成功，还要让 `readObject` 打出 RCE 才能读容器里的 `/flag`：

- classpath 只有 fastjson 1.2.48 + Spring 3.0.5 + commons-logging（无 CC/C3P0/Groovy/BeanUtils）；
- 黑名单挡掉 `Proxy` / `BadAttributeValueExpException` / RMI 一族 —— 恰好是 JDK8 现成链；
- 镜像 `jetty:9.4.49-jdk8` 的 JDK 已修掉 Jdk7u21 一类问题；
- 虽然 `resolveProxyClass` 没被覆盖（动态代理可能过），但**没有可用的触发链**。

结论：**以当前工具面（没有执行沙箱、不能编译/构造 payload）这一步做不了**。
这不是 planner 的问题，是能力边界（`docs/18 §6` 早就写过）。

---

## 5. 下一步选项

1. **把"过滤器旁路 + 差异信号"写进 worker prompt**（通用技巧，不是背答案）：
   `//path`、`;` 路径参数、`%2e`、大小写、后缀、绝对 URI；判定信号 = 响应来源变化
   （403 → 404-by-servlet / 500），不是 200。再跑一次，预期能到反序列化执行点。
2. **要真拿 flag**：需要一个可编译的 gadget 实验环境（JDK8 + 与目标同款 classpath + ysoserial），
   或者换一个能被已知链打穿的镜像 —— 后者等于改题。
3. **收尾**：把这次结果当作"M14 达标、M13/M15/M16 已验证"的验收，不再追 flag。

---

## 6. 复现命令

```powershell
# 建任务（serve 在跑）
POST /api/runs {"goal":"…拿到 /flag…","target":"http://node4.anna.nssnode.cn:26759/",
                "name":"CTF1-retest","timeout_seconds":1200,"worker_count":2}
POST /api/runs/42/attachments   # multipart file=Web-easy_api的附件.zip
POST /api/runs/42/control       # {"action":"start"}

# 手工旁路（授权靶机）
curl --path-as-is "http://node4.anna.nssnode.cn:26759//api/test"                       # 500 NPE
curl --path-as-is "http://node4.anna.nssnode.cn:26759//api/test?data=rO0ABXQABWhlbGxv" # 200 api test
```
