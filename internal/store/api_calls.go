package store

import "context"

// 115 API 调用量：按「天 + 账号」记。
//
// 为什么需要它：115 的风控额度按天算，用户看到的「今天调了多少次」如果只是
// 进程内计数，换个镜像就归零，等于永远显示一个偏小的数。所以计数落在库里，
// 内存里只维护「今天、这个账号」的累计值，按分钟批量灌回来。
//
// 口径是**覆盖**而不是累加：flush 时手里拿的是内存里的当日累计（不是增量），
// 累加会重复计数。同一个库只有一个进程在写，覆盖就够了。

// SetAPICalls 写入某天某账号的当日累计调用量。
func (s *Store) SetAPICalls(ctx context.Context, day string, accountID, n int64) error {
	if day == "" || accountID <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_calls (day, account_id, n) VALUES (?, ?, ?)
		 ON CONFLICT(day, account_id) DO UPDATE SET n = excluded.n`,
		day, accountID, n)
	return err
}

// APICallsOn 取某天所有账号的调用量，用于重启后接上今天的计数。
func (s *Store) APICallsOn(ctx context.Context, day string) (map[int64]int64, error) {
	out := map[int64]int64{}
	if day == "" {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, n FROM api_calls WHERE day = ?`, day)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return out, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// PruneAPICalls 清掉 keepFrom 之前的账。这张表一天一个账号一行，留着也没多少，
// 但两年前的账没有意义，顺手剪掉。
func (s *Store) PruneAPICalls(ctx context.Context, keepFrom string) error {
	if keepFrom == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_calls WHERE day < ?`, keepFrom)
	return err
}
