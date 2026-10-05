# swfkit · 通用 SWF 数值修改工具

纯 Go 单二进制工具，定位类似"Flash 游戏版 Cheat Engine / SWF 游戏修改大师"：

- **运行时修改**（Web UI）：内嵌 Ruffle（WebAssembly Flash 模拟器）直接运行游戏，
  通过挂钩 `WebAssembly` 实例化捕获其线性内存，提供 Cheat Engine 式的
  `scan → next → patch` 内存搜值/写值流程，改内存即时生效、无需重开游戏。
- **静态 ABC 字节码补丁**：解析 SWF 内的 DoABC（ActionScript 3 字节码），按数值扫描
  push 指令（pushbyte / pushshort / pushint / pushuint / pushdouble）并安全改写，
  支持常量池追加 + 指令重定向、窄指令升位（pushbyte → pushshort → pushint）、
  跳转与 try/catch 异常偏移自动重映射，并给出 `类名::方法名` 级归因。
- **.sol 存档编辑**：解析 Flash 本地共享对象（AMF0/AMF3，含 ByteArray、Date、
  ECMA 数组、typed object、AVM+ 切换），支持按点分路径改值、JSON 往返（键序保持）。
- **两种形态**：CLI 脚本化 + 本地 Web UI（`swfkit serve`，go:embed 单二进制）。

Web UI 使用**单个 Vue 3 全局构建文件**（vendored，`vendor/vue.global.prod.js`），
无 webpack/vite/Node 构建链；浅色侧边栏布局：运行时修改 / 静态补丁 / SOL 存档，
底部有控制台抽屉（三套命令随页面自动切换），内置 demo 文件（`/demo/`）可零门槛体验。
Ruffle 模拟器**内嵌于二进制**（0.6.0，gzip 预压缩分发，`/vendor/ruffle/`），
离线可用、秒级加载、版本可控；本地分发失败时自动回退 CDN。

## 快速开始

```bash
go build -o swfkit .
./swfkit serve --addr=127.0.0.1:7777     # 打开 http://127.0.0.1:7777，点"试用内置 demo"
```

### 运行时修改（Web UI）

1. 「运行时修改」页载入 SWF，三种方式任选，**自动降级**：
   - **文件选择 / 拖拽**：先按"同名 + 同大小"在持久化索引中智能定位磁盘真实路径，
     命中即走路径模式（外挂资源完整支持）；未命中降级为文件模式，
     并在检测到资源加载失败时给出明确指引
   - **本地路径**：服务端把游戏所在目录挂为静态根，相对资源完整解析
   - **内置 demo**：零门槛体验
2. **已知数值**：训练器表单输入 100 → 首次搜索 → 回游戏改变 → 再次搜索/变大了/变小了；
   **未知数值（血条/蓝条）**：点「未知初值扫描」建立全量候选（千万级、秒级完成），
   之后用 变大了/变小了/变了 缩小，无需知道精确值——引擎采用紧凑类型化数组
   （f64 8 对齐 ≈ 堆大小/8 个候选，i32 可选），缩小为 O(n) 原地压缩；
3. 地址表勾选 → 新值 → 「修改选中」直接写内存，**游戏立即生效**；
4. **⛶ 全屏**：整个舞台（游戏 + 悬浮半透明训练器面板）进入全屏，面板可收起，
   游戏全屏运行时随时搜值改值；Esc 退出。无 undo（内存不可回滚）。

已实测：国产 AS2 商业游戏（挖地小子，4.9MB + 外挂语言包/开场资源）文件选择后
自动定位、完整运行并进入训练器流程；AS3 游戏同理（AVM1/AVM2 数值最终都是
内存中的 f64/i32）。索引持久化于 `~/.config/swfkit/library.json`，
重启不丢；`POST /api/library/scan {root}` 可整库扫描（限深 6 层 / 2 万个上限）。

### 静态补丁

```bash
swfkit info game.swf                      # 概要与 ABC 模块统计
swfkit abc-list game.swf                  # 类与方法清单
swfkit abc-scan game.swf --value=100      # 扫描数值（类 CE 搜索）
swfkit abc-scan game.swf --value=3.14 --mode=double
swfkit abc-patch game.swf --value=100 --to=99999 --index=1 -o patched.swf
swfkit abc-patch game.swf --value=100 --to=99999 --all --where=Player   # 批量（先用 --where 收敛）

swfkit sol-dump save.sol                  # .sol → JSON（保持键序）
swfkit sol-set save.sol --path=player.bag.0 --value=magic_sword -o patched.sol
swfkit sol-build save.json -o save.sol    # JSON → .sol（配合 jq 可脚本化批改）
```

运行时说明：写内存基于对 Ruffle WASM 线性内存的原地改写（AS3 Number 为 f64、
小整数可能为 i32），地址稳定（WASM 内存扩容不搬页）；写错地址可能令游戏崩溃，
刷新页面重载即可。Ruffle 已内嵌本地分发，**离线时试玩/运行时同样可用**；
仅当本地分发异常且 CDN 回退也失败（如离线）时，试玩/运行时才不可用，
静态补丁与 SOL 编辑始终不受影响。
卡顿时可在工具栏降低渲染画质（实时生效）；控制台日志钩子仅在游戏加载窗口期
启用，检测完毕即恢复原始 console，运行期零拦截开销。

## 设计要点

- **自研纯 Go 核心**：SWF 容器（FWS/CWS，tag 流原样透传）与 AVM2 字节码
  （完整操作码表，逐项对照 [RABCDAsm](https://github.com/CyberShadow/RABCDAsm)
  的 opcodeInfo 表转录核对）均为自研实现，格式依据 Adobe《SWF 规范 v19》与
  《AVM2 Overview》公开文档。
- **踪迹式指令解码**（与 RABCDAsm 同思路）：从入口与全部跳转目标遍历可达代码，
  死代码 / 内嵌数据表以 raw 伪指令原样透传，可处理混淆器产出的非常规代码；
  "跳入指令中间"等异常一律硬报错，绝不静默产出坏文件。
- **补丁安全性**：优先复用常量池已有等值条目，否则追加新条目并仅重定向目标指令，
  绝不原地改共享常量；操作数变长时整个方法体重编码并同步修正异常边界偏移。
- **格式冷知识**（都踩过）：`class_count` 不存储于 ABC 文件（规范规定其必须等于
  instance_count，直接复用）；`s24` 分支偏移需要符号扩展；分支偏移相对指令末尾而
  lookupswitch 偏移相对指令起点；metadata 条目是"全部 keys 之后再全部 values"；
  trait metadata 索引为 0 基。

## 已验证

- 单元测试：FWS/CWS 与 ABC 往返字节一致；分支 / lookupswitch / try-catch /
  死代码透传 / AMF0+AMF3 / SOL JSON 往返等。
- 真实样本：Flex SDK（compc）编译的 flexunit `library.swf`（238 个 DoABC 模块）
  全量解析、两轮序列化字节一致、扫描归因与补丁正确。设置
  `SWFKIT_REAL_SWF=<path>` 运行 `go test -run RealWorld ./internal/swf/` 复验。
- 浏览器端到端：Vue 页面挂载、控制台三模式命令、假 WASM 内存的
  scan（含 4/8 字节两种对齐）→ next 缩小 → patch 实写校验、
  静态补丁 demo 全流程（扫描归因→补丁→新值复扫）、SOL 树渲染与改值。

## 已知边界（v1）

- 不支持 ZWS（LZMA）压缩、AS1/AS2 时代（无 DoABC）的 SWF、Externalizable 类、
  加密 / 强混淆 SWF（安全脱壳不在目标内）。
- ABC 写回为语义等价重建：常量池引用一律内联展开、AMF3 专属类型经 AVM+ 切换
  落盘为 AMF0 等价形式，不保证与原文件逐字节一致。
- 搜索为精确值匹配；"未知初始值 / 递增搜索"类 CE 高级特性暂未实现。

## 目录

```
main.go             入口
internal/swf/        SWF 容器（头、RECT 位流、tag 流、zlib）
internal/abc/        AVM2：常量池、结构、指令编解码、DoABC 封装
internal/patch/      数值扫描 / 补丁引擎 / 归因
internal/sol/        AMF0/AMF3、.sol 容器、有序 JSON
internal/cli/        cobra 子命令
internal/web/        Web UI（API + Vue 全局构建 + go:embed，无前端构建链）
  └─ static/vendor/  vue.global.prod.js 与 ruffle/（0.6.0 自托管，gzip 预压缩）
  └─ static/demo/    内置 demo 游戏与存档
internal/testutil/   测试样本构建器（手汇字节码）
```

仅用于本地单机游戏存档与资源的个人修改。
