# vexport

> 类型感知的 Go 声明提取器——把任意类型、函数或方法从包（及其依赖）中抽出来，重写 import，沿 `//go:build` 约束切分，并证明结果依然能编译。

`vexport` 用 `go/types` 读你的代码，而不是正则。你按名字告诉它*要什么*，它自己算出让这个子集能独立编译所需的全部传递闭包——声明、方法、构造函数、常量、import——包括从其他包拉进来的代码。

---

## 为什么需要它

有时候你只需要包的一部分，而不是整个包：

- 从依赖里 vendor 一个类型，但不要它的 `init()` 副作用，也不要它的 `main` 包。
- 从一个带五种 `//go:build` 变体的库里，切出平台无关的核心。
- 从 monorepo 里几个埋得很深的工具函数生成一个自包含的辅助包。
- 为插件、测试脚手架或代码生成器产出一份 build-tag 正确的包副本。

手工做这件事很容易出错：漏掉一个 helper、忘掉一个常量、搞坏一个 dot-import，或者破坏 `GOOS` 特定的代码。`vexport` 用完整的类型信息来做，并用真实工具链验证输出。

---

## 特性

- **类型驱动提取。** 声明通过 `go/types` 对象识别，所以 `Type.Method` 选择器、嵌入字段、iota 块、多名 `var`/`const` spec 都能正确处理。
- **跨包递归内联**（`-follow`），带深度限制（`-follow-depth`）和显式排除模式。标准库默认不跟随——`-follow std` 跟随全部，`-follow fmt` 这类精确路径按需跟随单个 std 包。
- **构建矩阵支持。** 按 target 提取一次（`-targets linux/amd64,windows/amd64,darwin/arm64+tag`），或者让 `-targets auto` 从 `//go:build` 行和文件名后缀自动推导最小覆盖集。
- **约束感知输出。** 需要时生成正确的 `//go:build` 表达式（以及旧的 `// +build` 形式），可移植的文件保持无 tag。
- **导入重写。** 用 `-rewrite old=new` 或 `-rewrite example.com/old/=example.com/new/` 重命名或迁移 import。
- **冲突策略。** `keep`（保留为 import）、`prefix`（重命名为 `PkgName+Name`）、`error`。
- **安全护栏。** 拒绝覆盖不是它生成的文件，去重输出文件名，写入前可用 `go build` / `go vet` 验证结果。
- **可编程流水线。** 五个阶段（`load → resolve → emit → verify → write`）都是接口，外加 middleware 支持，可以嵌入或扩展。
- **机器可读报告。** `-json` 输出稳定、排好序的报告，每个生成文件带 SHA-256。

---

## 安装

```sh
go install github.com/yourname/vexport@latest
```

或者从源码构建：

```sh
git clone https://github.com/yourname/vexport
cd vexport
go build -o vexport .
```

需要 Go 1.22+（用到 `strings.SplitSeq` 和 range-over-func）。

---

## 快速开始

把 `User` 类型（及其方法、构造函数和依赖）提取到一个新包：

```sh
vexport run -dir . -pkg ./internal/user -names User -o ./gen
```

提取当前模块的全部导出声明（不含标准库和第三方包）：

```sh
vexport run -dir . -pkg ./user -all -follow module -o ./gen
```

为三个平台提取并逐个验证：

```sh
vexport run -dir . -pkg ./internal/user -names User,ParseUser \
    -targets linux/amd64,windows/amd64,darwin/arm64 \
    -o ./gen -verify
```

只看看会提取什么，*不*写任何东西：

```sh
vexport plan -dir . -pkg ./internal/user -all
```

---

## 用法

```
vexport run  [flags]      提取、渲染、验证并写入
vexport plan [flags]      仅加载并解析，打印计划
vexport version           打印版本
vexport help              打印完整帮助
```

### 输入

| 旗标 | 说明 |
| --- | --- |
| `-dir DIR` | 模块或包目录（默认 `.`）。 |
| `-pkg PATTERN` | 要加载的包，例如 `./internal/user` 或导入路径。 |
| `-names LIST` | 逗号分隔的声明。允许 `Type.Method`；`*` 表示所有导出声明。 |
| `-all` | `-names *` 的简写。 |
| `-jobs N` | 并行加载的 target 数。`0` = 自动（CPU 数），`1` = 串行。 |

### 构建矩阵

| 旗标 | 说明 |
| --- | --- |
| `-targets LIST` | `linux/amd64,windows/amd64,darwin/arm64+tag,tag`，或 `auto` 扫描目标包并推导最小覆盖集。 |

### 选择

| 旗标 | 说明 |
| --- | --- |
| `-methods` | 复制所选类型的方法（默认 `true`）。 |
| `-ctors` | 复制所选类型的构造函数（默认 `true`）。 |
| `-follow LIST` | 递归内联依赖：`module`、`all`、`std`、`!exclude`、`github.com/x/y`、`github.com/x/**`，或 `fmt` 这样的精确 std 包路径。 |
| `-follow-depth N` | 跟随时跨包最大跳数（`-1` = 不限，`0` = 关闭，默认 `8`）。 |
| `-rewrite LIST` | 导入路径重写：`old=new` 或 `example.com/old/=example.com/new/`。 |
| `-conflict MODE` | `keep`（默认）、`prefix` 或 `error`。 |
| `-max-decls N` | 内联声明的硬上限（默认 `5000`，`0` = 不限）。 |
| `-strict` | 如果结果仍 import 当前模块之外的第三方包则失败（退出码 1）。 |

### 输出

| 旗标 | 说明 |
| --- | --- |
| `-o PATH` | 输出文件、目录，或 `-` 表示 stdout。 |
| `-split MODE` | `auto`（默认）、`none` 或 `target`。 |
| `-suffix S` | 文件名后缀，追加在包名之后。 |
| `-short-names` | 当 GOOS 全局唯一时省略架构后缀（`name_linux.go` 而非 `name_linux_amd64.go`）。 |
| `-split-by-package` | 按来源包拆分输出；内联包以包名作文件名前缀。 |
| `-keep-tags` | 始终写入 `//go:build` 行，即使是一个可移植文件。 |
| `-force` | 覆盖不是 `vexport` 生成的文件。 |
| `-dry-run` | 渲染并验证，但不写入。 |
| `-allow-partial` | 跳过不存在的 `-names`，其余照常输出。 |

### 验证

| 旗标 | 说明 |
| --- | --- |
| `-verify` | 用真实 Go 工具链 build（或 vet）生成代码。 |
| `-verify-cmd CMD` | `build`（默认）或 `vet`。 |
| `-verify-targets` | 验证每个 target，而不只是宿主机（默认 `true`）。 |
| `-verify-keep` | 保留临时验证包。 |
| `-verify-timeout D` | 单次构建超时（默认 `3m`）。 |

### 报告

| 旗标 | 说明 |
| --- | --- |
| `-json` | 在 stdout 输出机器可读报告。 |
| `-q` | 抑制提示和警告。 |
| `-v` | 记录每个流水线阶段。 |

---

## 标准库跟随

标准库默认**不**跟随——`strings`、`fmt`、`time` 这些保持为 import。需要内联时，有两档选择：

**精确跟随某个 std 包**（推荐，闭包可控）：

```sh
vexport run -dir . -pkg ./internal/util -names Sanitize \
    -follow fmt -o ./gen
```

`fmt` 及其它被 `Sanitize` 引用的 std 包里的声明会被复制进来，`fmt.` 限定符被剥离。`strings` 没被点名也没被闭包触及，保持 import。

**跟随整个标准库**（重，慎用）：

```sh
vexport run -dir . -pkg ./internal/util -names Sanitize \
    -follow std -o ./gen
```

所有被闭包触达的 std 包都会内联。`fmt` 会牵出 `internal/fmtsort`、`reflect`，`reflect` 又牵出 `internal/abi`、`unsafe`……一个 `-names Foo -follow all -follow std` 很容易撞上 `-max-decls`（默认 5000）或卡在类型检查里。配合 `-follow-depth` 和 `-max-decls` 兜底。

**`-follow all` 不碰 std。** 想批量跟随 module 外的包、又包含标准库，把 `std` 显式写上：

```sh
vexport run -dir . -pkg ./cmd/app -names Config \
    -follow all -follow std -max-decls 2000 -o ./gen
```

`std` 和精确路径可以混写，`-follow fmt,encoding/json` 等价于 `-follow fmt -follow encoding/json`。

---

## 示例

### 提取单个类型及其依赖

```sh
vexport run -dir . -pkg ./internal/model -names User -o ./gen
```

`User` 会连同它的方法、返回它的构造函数、以及这些声明依赖的一切一起复制。import 从类型检查器收集，而不是从文本里——所以注释和字符串里提到 `pkg.X` 不会被误判。

### 内联一个 helper 模块

```sh
vexport run -dir . -pkg ./cmd/app -names Config \
    -follow module -o ./internal/config -verify
```

`-follow module` 会把从 `Config` 出发、在当前 Go module 内可达的一切都拉进来。标准库和第三方包保持 import。`-verify` 在写入前于宿主机上 build 结果。

### 处理平台特定矩阵

```sh
vexport run -dir . -pkg ./net -names Conn \
    -targets linux/amd64,darwin/arm64,windows/amd64 \
    -o ./gen -split auto
```

用 `-split auto`（默认）：

- 如果每个 target 渲染出的文本完全相同，写一个可移植文件。
- 否则，每种不同的渲染结果写一个文件，各自带上正确的 `//go:build` 表达式。

### 从源码发现 target

```sh
vexport run -dir . -pkg ./internal/platform -all -targets auto -o ./gen
```

`-targets auto` 扫描 `-pkg` 目录，读 `//go:build` 行和 `_GOOS_GOARCH` 文件名后缀，用 `go/build` 验证候选，然后选出最小覆盖集。自定义 tag（不是已知 GOOS/GOARCH 的）会被报告，必须显式启用：

```sh
vexport run -dir . -pkg ./internal/platform -all -targets 'linux/amd64+premium' -o ./gen
```

### 重命名内联包以避免冲突

```sh
vexport run -dir . -pkg ./app -names Service \
    -follow github.com/acme/util -conflict prefix -o ./gen
```

如果 `app` 已有 `Logger`，而 `util` 也定义了 `Logger`，内联进来的那个会重命名为 `UtilLogger`，复制代码里所有调用点都跟着改。

### 作为库使用

```go
import "github.com/yourname/vexport"

cfg := vexport.DefaultConfig()
cfg.Dir = "."
cfg.Pattern = "./internal/user"
cfg.Names = []string{"User"}
cfg.Targets = []vexport.Target{{GOOS: "linux", GOARCH: "amd64"}}
cfg.Verify.Enabled = true

p := vexport.New(cfg)
p.Use(func(next vexport.StageFunc) vexport.StageFunc {
    return func(ctx context.Context, st *vexport.State) error {
        if err := next(ctx, st); err != nil {
            return err
        }
        // 策略：拒绝输出超过 4 个文件
        if len(st.Files) > 4 {
            return fmt.Errorf("refusing to emit %d files", len(st.Files))
        }
        return nil
    }
})

st, err := p.Run(context.Background())
```

流水线暴露五个接口——`Loader`、`Resolver`、`Emitter`、`Verifier`、`Writer`——每个都可以通过 `WithLoader`、`WithResolver` 等替换。

---

## 工作原理

```
load    → []*Program      每个 target 一个已加载、类型检查、建好索引的包图
resolve → []*Selection    种子 + 递归依赖闭包 + 源码编辑
emit    → []File          按 build 约束分组渲染出的文件
verify  → []VerifyResult  结果能编译的证明
write   → []File          磁盘上的文件（或 stdout）
```

1. **Load。** 每个 target 跑自己的 `go list`，拿到自己的 `token.FileSet` 和自己的 `go/types` 信息。target 之间完全独立，所以加载是尴尬并行的。
2. **Resolve。** 种子按名字在包索引里查。每个引用解析成 `types.Object`；如果对象所在的包匹配 `-follow`，它的声明就进入闭包。`//go:build` 约束跟着声明走。`-follow` 的匹配分四种：精确 include（`fmt`、`github.com/x/y`）、通配（`github.com/x/**`）、`module`（当前 module 的所有子包）、`std`（全部标准库）。
3. **Emit。** 声明通过复制源码区间并应用记录的编辑来渲染：剥掉内联的限定符、重命名别名、给 dot-import 的标识符加限定、应用冲突重命名。variant 按文本相同分组，每组得到从声明源码约束*与*运算推导出的 build 约束。
4. **Verify**（可选）。文件写进模块内的临时包，用真实工具链 build，然后删除。`package main` 用 `go test -c` 编译，所以不要求 `func main`。
5. **Write。** 带安全检查：绝不覆盖不是它生成的文件，绝不写重复文件名，绝不把多个文件写进 stdout。

### 正确性保证

- **`-follow-depth` 和 `-max-decls`** 阻止失控内联。
- **`init()` 块会被尊重。** 被跟随的包如果有 `init` 函数，会保留为 import——内联它会静默丢掉副作用。
- **一个包要么整体内联，要么整体 import。** 如果某个包里有一个声明必须保留为 import（`-conflict keep` 下的名字冲突、`init`、或类型错误），整个包都保留为 import。部分内联会造出两个类型标识。
- **`-conflict prefix` 会重写调用点**，不只是声明本身。嵌入结构体字段作为特例处理，因为它们的名字派生自类型名。
- **iota 块是原子的。** 选中块里一个常量会复制整个块，它的输出名字作为一个单元预留。

---

## 退出码

| 码 | 含义 |
| --- | --- |
| `0` | 成功。 |
| `1` | 加载或类型错误。 |
| `2` | 某个 `-names` 条目未找到。 |
| `3` | 输出错误。 |
| `4` | 验证失败。 |
| `5` | 用法错误。 |

---

## JSON 报告

`-json` 在 stdout 输出稳定、排好序的报告：

```json
{
  "schema": 1,
  "tool": "vexport 2.0.0",
  "generated_at": "2026-11-15T12:51:57Z",
  "duration_ms": 842,
  "config": { "...": "..." },
  "declarations": [
    { "kind": "type", "name": "User", "origin": "(target)", "target": "linux/amd64" }
  ],
  "files": [
    {
      "name": "user_linux_amd64.go",
      "package": "user",
      "constraint": "linux && amd64",
      "targets": ["linux/amd64"],
      "bytes": 1842,
      "sha256": "d1c3…"
    }
  ],
  "verify": [{ "target": "linux/amd64", "command": "build", "ok": true, "millis": 219 }],
  "stages": [{ "name": "load", "millis": 401 }, { "name": "resolve", "millis": 12 }],
  "warnings": [],
  "notes": [],
  "missing": []
}
```

所有集合都排好序，所以报告在 CI 里 diff 很干净。

---

## 限制

- **`-targets auto` 只扫描根包目录。** `-follow` 跨包引入的约束在发现阶段不可见。如果 `-follow` 会触及平台特定代码，请显式传 `-targets` 列表。
- **自定义 build tag 无法推断。** 像 `integration`、`premium` 这样的 tag 会被报告但不会启用；用 `linux/amd64+premium` 显式启用。
- **`cgo` 文件尊重宿主工具链。** `-targets auto` 使用宿主的 `CgoEnabled` 设置；显式 target 仍可选择无 cgo 构建。
- **验证在源模块内运行。** 它写进 `go.mod` 父目录下的临时目录，除非传 `-verify-keep` 否则会删除。不碰其他任何东西。

---

## 许可证

MIT —— 见 [LICENSE](./LICENSE)。