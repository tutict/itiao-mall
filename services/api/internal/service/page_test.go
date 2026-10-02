package service

import "testing"

// 这一组盯分页的两道防线。
//
// 分页参数是"永远不报错"的那一类：非法值一律被夹回合法区间。好处是客户端不用
// 处理 400，坏处是**夹错了也没人知道** —— 客户端请求第 2147483648 页拿到第 1 页，
// 界面上看不出任何异常，只能靠用户投诉。
//
// 两道防线各管一段：
//   - derefInt32（httpapi 层）：int64 → int32 的收窄，会截断
//   - normalizePage（本文件）：把非法值夹回区间
//
// 任何调用方只要绕过 oapi-codegen（未来加内部 RPC、换 codegen），就只剩第二道；
// 所以第二道必须自己能挡住，不能假设"上游已经校验过了"。

func TestNormalizePage_ClampsToValidRange(t *testing.T) {
	cases := []struct {
		name           string
		page, pageSize int32
		wantLimit      int32
		wantOffset     int32
	}{
		{"正常值", 1, 20, 20, 0},
		{"第二页", 2, 20, 20, 20},
		{"第 100 页", 100, 20, 20, 1980},
		{"page=0 夹到 1", 0, 20, 20, 0},
		{"page 为负夹到 1", -5, 20, 20, 0},
		{"pageSize=0 用默认", 1, 0, 20, 0},
		{"pageSize 过大夹到 100", 1, 9999, 100, 0},
		{"pageSize 为负用默认", 1, -3, 20, 0},

		// 溢出：page 是 int32 极值时 (page-1)*pageSize 原本会溢出成负数
		// （(2147483647-1)*100 = -200）。现在先除后乘夹到 maxPage，
		// 期望值 = math.MaxInt32/pageSize + 1 对应的最大 offset。
		{"page=int32 极值夹到最大可表示页", 2147483647, 20, 20, 2147483640},
		{"page=int32 极值 + pageSize=100", 2147483647, 100, 100, 2147483600},
		{"page=1000000 不溢出", 1000000, 100, 100, 99999900},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit, offset := normalizePage(tc.page, tc.pageSize)
			if limit != tc.wantLimit {
				t.Errorf("limit = %d，期望 %d", limit, tc.wantLimit)
			}
			if offset != tc.wantOffset {
				t.Errorf("offset = %d，期望 %d", offset, tc.wantOffset)
			}
			if offset < 0 {
				t.Errorf("offset 为负（%d）会被 PostgreSQL 当成错误，SQL 只会收到负数 OFFSET", offset)
			}
		})
	}
}

// TestNormalizePage_OffsetNeverExceedsInt32 单独钉住溢出边界。
//
// 上面那条是具体值；这条是不变量：无论输入什么，offset 都不能越界或为负。
// 越界的 offset 传到 pgx 会被当参数类型错误，整条查询失败。
func TestNormalizePage_OffsetNeverExceedsInt32(t *testing.T) {
	inputs := []int32{-2147483648, -1, 0, 1, 2, 100, 100000, 2147483646, 2147483647}
	sizes := []int32{-1, 0, 1, 20, 100, 101, 2147483647}

	for _, p := range inputs {
		for _, s := range sizes {
			limit, offset := normalizePage(p, s)
			if offset < 0 {
				t.Errorf("normalizePage(%d, %d) offset 为负: %d", p, s, offset)
			}
			if limit < 1 {
				t.Errorf("normalizePage(%d, %d) limit < 1: %d", p, s, limit)
			}
			if limit > 100 {
				t.Errorf("normalizePage(%d, %d) limit 超过上限 100: %d", p, s, limit)
			}
		}
	}
}
