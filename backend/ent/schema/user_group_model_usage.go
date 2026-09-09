package schema

import (
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// UserGroupModelUsage 记录 (user, group, 模型配额规则) 维度的三窗口用量。
//
// 限额存在 groups.model_quotas（JSONB 配置）中，本表只存用量：
// Group 已随 api key auth cache 加载，判定时读限额零额外查询。
//
// rule_key 是规则原文的归一形式（如 "claude-opus*"），不是具体模型名——
// 同系列模型共享一份额度。
type UserGroupModelUsage struct {
	ent.Schema
}

func (UserGroupModelUsage) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "user_group_model_usage"},
	}
}

func (UserGroupModelUsage) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (UserGroupModelUsage) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("user_id"),
		field.Int64("group_id"),
		field.String("rule_key").
			MaxLen(200).
			NotEmpty().
			Comment("命中的配额规则原文（归一后小写），非具体模型名"),

		// 当前窗口已用量（USD，preflight 时与配置中的 limit 比较）
		field.Float("daily_usage_usd").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),
		field.Float("weekly_usage_usd").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),
		field.Float("monthly_usage_usd").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),

		// 窗口起点（NULL = 首次还未初始化）
		field.Time("daily_window_start").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("weekly_window_start").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("monthly_window_start").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (UserGroupModelUsage) Indexes() []ent.Index {
	return []ent.Index{
		// 软删除友好：只对未删记录唯一。落账的 ON CONFLICT 依赖此索引。
		index.Fields("user_id", "group_id", "rule_key").
			Unique().
			StorageKey("usergroupmodelusage_user_group_rule_uq").
			Annotations(entsql.IndexWhere("deleted_at IS NULL")),
		index.Fields("user_id", "group_id"),
		index.Fields("deleted_at"),
	}
}
