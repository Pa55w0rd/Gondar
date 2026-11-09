package db

import "fmt"

// TaskScenePreset only supplies defaults for the existing task-create fields.
// It does not introduce another task state machine or relax tool permissions.
type TaskScenePreset struct {
	InputLabel       string `json:"input_label"`
	InputPlaceholder string `json:"input_placeholder"`
	InputKind        string `json:"input_kind"` // text, host, domains, ips
	MaxTargets       int    `json:"max_targets"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	SeedFirstIntent  bool   `json:"seed_first_intent"`
	CoverageEnabled  bool   `json:"coverage_enabled"`
}

type builtinTaskScene struct {
	key, name, description, goal string
	preset                       TaskScenePreset
}

const sceneCardGuidance = `
工作方式：planner 先依据现有事实拆出少量有前置关系的意图；worker 只执行领到的核验卡。每张卡写明对象、前置条件、待验证假设、最小证明、停止条件、证据出处；结果使用现有 fact/finding/hint 记录，不另造状态。
工具选择：只使用本轮实际可见的工具。延迟工具先搜索确认再调用；缺工具、账号或环境时记录具体缺口，不编造调用或成功结果。先复用已有流量与证据，避免仅为凑证据重复请求。
证据要求：区分已验证、未验证、被防护阻断和环境不可用；观察不等于推断，调用过不等于成功。保留必要的请求/响应、文件行号或命令输出与来源ID，敏感值脱敏。结束前交付覆盖清单、确认结果、未测面和阻塞原因。
预算说明：创建表单会应用本场景的建议时限，可在提交前调整；任务时限由现有引擎执行。下述请求预算、逐目标边界和方法约束是任务指导，不是网络级硬隔离。`

var builtinTaskScenes = []builtinTaskScene{
	{
		key: "single_web", name: "单域名 Web/API",
		preset: TaskScenePreset{"精确主机或入口 URL", "https://console.example.test", "host", 1, 1800, false, true},
		description: `场景：单域名 Web/API。唯一目标：{{target}}。
范围：只检查该输入的精确主机与显式端口，不把 eTLD+1、兄弟域名、共享IP或第三方登录/静态资源当作授权。遇到跨主机重定向先停在边界记录，不自动跟随探测。
先做入口与接口梳理，再按真实输入和业务上下文选择核验；API与页面共用证据，禁止只按扫描告警上报。每轮最多3张可执行核验卡；默认并发不超过2、每主机不超过2请求/秒，出现429、持续5xx或时延显著上升时停止主动请求并说明。
保留同一入口的基线/对照及影响证明；没有账号时明确登录后覆盖缺口，不把登录页响应判定为业务已测。` + sceneCardGuidance,
		goal: `对 {{target}} 完成精确主机范围内的 Web/API 核验，交付入口/接口覆盖清单、可复核的确认漏洞与证据、已排除假设及未验证面。不得为追求数量扩域；未实际验证的项目不能计为已覆盖。`,
	},
	{
		key: "authenticated_business", name: "登录态业务权限",
		preset: TaskScenePreset{"业务入口与允许对照的账号角色", "https://app.example.test；角色A/角色B，仅填写角色标识，凭据另配", "text", 1, 2700, false, true},
		description: `场景：登录态业务权限。授权对象与角色：{{target}}。
使用任务登录配置或受控会话，不在描述、图节点或报告写密码、TOTP、Cookie明文。先验证会话有效性与当前角色，再建立“主体×操作×资源归属”的权限矩阵；缺少可对照账号时记录未测，不伪造另一角色结果。
围绕授权的读写操作检查对象级/功能级权限和业务状态前置，至少保留合法基线与越权对照。默认只用受控测试数据，不执行真实付款、批量发送、不可逆删除等有业务后果的操作；需要额外权限或副作用时交给现有审批。
每轮最多3张核验卡，默认每账号串行、每主机不超过1请求/秒；会话过期或验证码阻塞先恢复登录前置，不持续盲试。` + sceneCardGuidance,
		goal: `对 {{target}} 建立并核验业务权限矩阵，确认或排除对象/功能越权与关键业务前置缺陷。每个结论包含角色、资源归属、合法基线、对照和实际影响；缺账号、缺权限或未执行的流程单列。`,
	},
	{
		key: "source_audit", name: "源码审计",
		preset: TaskScenePreset{"源码路径/仓库与审计范围", "D:\\project\\src；分支/提交；需要关注的模块", "text", 1, 3600, false, false},
		description: `场景：源码审计。代码对象：{{target}}。
先确认实际可读源码位置、语言、固定提交/版本、入口和依赖；没有源码或关键依赖时列出缺口。按入口到危险操作追踪数据流与校验，不把关键字命中当作漏洞；复用现有搜索、读取和证据附件，不反复全仓遍历。
每轮只派少量明确模块/链路，核验卡包含输入来源、传播路径、校验及危险操作、触发条件和文件行号。动态验证仅使用明确提供的本地测试/隔离环境；不得因代码里出现生产URL自动访问外部系统。
目标是有出处的代码结论和覆盖缺口。构建/测试是否执行、实际结果与静态推断分别记录，未验证依赖或环境条件不可写成已利用。` + sceneCardGuidance,
		goal: `对 {{target}} 交付可追溯的源码审计结果：固定版本、已审入口/模块、完整数据流与校验分析、确认问题的文件行号和触发条件，以及未审/未运行范围。无证据的猜测不登记为确认漏洞。`,
	},
	{
		key: "finding_retest", name: "指定漏洞复测",
		preset: TaskScenePreset{"漏洞编号、原证据与指定复测目标", "finding_id=123；原始复现/修复说明；https://app.example.test", "text", 1, 1200, false, false},
		description: `场景：指定漏洞复测。复测对象：{{target}}。
只围绕指定漏洞和原始前置条件复测，不扩展为整站扫描。先读取原报告、真实finding_id/证据、受影响版本和修复说明；找不到原始证据时先说明，不猜编号或编造基线。
复用原合法基线和最小复现，比较修复前后语义及必要回归路径；默认串行、每主机不超过1请求/秒。使用现有复测/报告接口记录结果，不重复创建同一漏洞。
结论必须区分仍存在、已修复、部分修复、环境变化导致无法判断；一次超时、403或登录失效不能直接等同已修复。` + sceneCardGuidance,
		goal: `对 {{target}} 给出逐漏洞复测结论与对照证据，准确区分仍存在、已修复、部分修复和无法判断；列出版本/环境差异，更新现有记录而非重复报洞，不扩大目标范围。`,
	},
	{
		key: "asset_inventory", name: "资产梳理",
		preset: TaskScenePreset{"已授权资产或资料清单", "域名/IP/系统清单；允许使用的数据源与发现深度", "text", 100, 2700, false, true},
		description: `场景：资产梳理。已授权输入：{{target}}。
先固定输入清单与允许的数据源，优先复用现有资产、提供的资料和已采集流量；发现的关联对象只记来源与归属置信度，不因共享IP、证书、备案或同根域名自动获得测试授权。
每个资产记录规范化标识、类型、来源、可达性/服务的实际证据和待确认项，幂等合并避免重复。主动确认只针对清单明确允许的对象，默认每主机不超过1请求/秒；不执行漏洞利用或凭据尝试。
按输入资产给出已确认/未确认/不可达的清单；企业归属推断、可达与授权是不同属性，不得混淆。` + sceneCardGuidance,
		goal: `对 {{target}} 形成去重、可追溯的资产清单和关系说明，逐项标明实际证据、归属不确定性与覆盖缺口。新发现关联对象单列待授权，不把资产测绘结果包装为漏洞或自动扩展测试范围。`,
	},
	{
		key: "ctf", name: "CTF",
		preset: TaskScenePreset{"题目、附件位置与隔离目标", "题目说明；本地附件路径；靶场入口与允许交互范围", "text", 1, 1800, true, false},
		description: `场景：CTF。题目与隔离对象：{{target}}。
此场景启用“优先直接开跑”：短单步骤任务可生成首个意图，复杂或多目标说明仍先交由planner规划，不代表跳过审批或获得额外工具。先确认题面、附件/靶场入口、允许的交互边界与flag格式；缺少附件或环境时说明，禁止把真实第三方系统当作靶场。
worker先处理一个最小可验证方向，记录尝试、输出和中间产物；需要多个依赖步骤时交由既有planner/worker继续推进。不要为得分伪造flag或把报错/空响应当作成功。
默认30分钟建议预算；只在题目限定环境内验证，达成后保留复现与关键证据，不继续无关探索。` + sceneCardGuidance,
		goal: `在 {{target}} 的明确题目边界内完成可复现求解，给出真实flag/达成结果、关键步骤与证据；若未完成，说明已验证事实、剩余假设和具体阻塞，不虚构成功。`,
	},
	{
		key: "batch_domains", name: "批量域名",
		preset: TaskScenePreset{"精确域名清单（每行一个，最多100个）", "api.example.test\nconsole.example.test", "domains", 100, 5400, false, true},
		description: `场景：批量域名。规范化、去重后的精确主机清单：
{{target}}
仅上述逐行主机属于本任务输入，不自动展开根域、通配、兄弟域、证书关联或共享IP。前端只负责清单格式校验/去重；请求执行仍需遵守任务约束与工具审批，清单不是网络沙箱。
先做低成本入口确认和分类，再按同类站点复用方法；每轮最多3张核验卡，默认总并发不超过3、每主机不超过1请求/秒。任何流量、证据、漏洞都必须回连到具体输入主机，不将一个站点的结论套给整批。
逐目标记录已测入口、确认结果、未测面和失败原因；429/持续5xx/显著延迟时停止该目标，不为追求全覆盖重复盲扫。` + sceneCardGuidance,
		goal: `对以下精确主机逐项形成独立覆盖与证据清单，不扩域、不以一项代表整批：
{{target}}
交付每个目标的确认结果、未验证项和失败原因；统计以去重后的输入为分母，未测/不可达不能算已测，无归属证据的告警不算有效发现。`,
	},
	{
		key: "batch_ips", name: "批量 IP",
		preset: TaskScenePreset{"精确IP清单（每行一个，最多100个）", "192.0.2.10\n2001:db8::10", "ips", 100, 3600, false, true},
		description: `场景：批量 IP。规范化、去重后的精确地址清单：
{{target}}
只针对清单地址，输入不接受CIDR自动展开；端口与协议确认限于明确允许的范围，未提供时先记录需要确认的端口范围，不默认全端口。反向域名、旁站、网段邻居和内网关联仅记为待确认，不自动转为测试对象。
先按地址/协议建立最小服务清单，再依据实际服务派核验卡；默认总并发不超过3、每地址串行，避免同一服务重复探测。每个发现保留IP、端口、协议、前置条件与实际输出，虚拟主机/证书不能替代归属证明。
逐地址记录服务、可达性、证据和未测端口/协议；无响应不能直接证明安全或已覆盖全部服务。` + sceneCardGuidance,
		goal: `对以下精确IP逐项交付服务/核验覆盖与证据，不扩网段、不自动转测关联域名：
{{target}}
以去重输入为分母，清楚列出已核验服务、未测范围、不可达和阻塞；确认漏洞必须绑定具体IP/端口/协议，不能把工具告警当作已证明影响。`,
	},
}

func taskScenePreset(key string) *TaskScenePreset {
	for _, scene := range builtinTaskScenes {
		if scene.key == key {
			preset := scene.preset
			return &preset
		}
	}
	return nil
}

// seedBuiltinTaskScenes is intentionally not gated by a one-time settings flag:
// upgrades must add newly introduced/missing scenes. Only managed builtin keys
// are updated; a user's editable templates are never overwritten.
func (d *DB) seedBuiltinTaskScenes() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, scene := range builtinTaskScenes {
		if _, _, err := normalizeTaskTemplateInput(TaskTemplateInput{Name: scene.name, Description: scene.description, Goal: scene.goal}); err != nil {
			return fmt.Errorf("scene %s: %w", scene.key, err)
		}
		// A pre-upgrade user template may already own the reserved-looking name.
		// Choose another internal nkey instead of overwriting it or failing startup.
		nkey := "builtin:" + scene.key
		for suffix := 0; ; suffix++ {
			if suffix > 0 {
				nkey = fmt.Sprintf("builtin:%s:%d", scene.key, suffix)
			}
			var occupied bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_templates WHERE nkey=$1 AND builtin_key IS DISTINCT FROM $2::text)`, nkey, scene.key).Scan(&occupied); err != nil {
				return err
			}
			if !occupied {
				break
			}
		}
		_, err := tx.Exec(`INSERT INTO task_templates(name,nkey,description,goal,intercept_rules,builtin_key)
VALUES ($1,$2,$3,$4,'[]',$5)
ON CONFLICT (builtin_key) WHERE builtin_key IS NOT NULL DO UPDATE
SET name=EXCLUDED.name, description=EXCLUDED.description, goal=EXCLUDED.goal
WHERE task_templates.name IS DISTINCT FROM EXCLUDED.name
   OR task_templates.description IS DISTINCT FROM EXCLUDED.description
   OR task_templates.goal IS DISTINCT FROM EXCLUDED.goal`,
			scene.name, nkey, scene.description, scene.goal, scene.key)
		if err != nil {
			return fmt.Errorf("scene %s: %w", scene.key, err)
		}
	}
	return tx.Commit()
}
