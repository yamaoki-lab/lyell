package itemstore

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// LinkType は, revision のリンクの型. 名前と major 番号で決まる (0001.sql の link_types の主キー).
type LinkType struct {
	Name  string
	Major int
}

// この版の Lyell が知っている4つの型. それぞれの意味とフラグは 0001.sql の link_types に書く.
var (
	LinkParent      = LinkType{Name: "parent", Major: 1}
	LinkRequired    = LinkType{Name: "required", Major: 1}
	LinkDerivedFrom = LinkType{Name: "derived_from", Major: 1}
	LinkBelongsTo   = LinkType{Name: "belongs_to", Major: 1}
)

func (t LinkType) known() bool {
	return t == LinkParent || t == LinkRequired || t == LinkDerivedFrom || t == LinkBelongsTo
}

// linkTypeName は, 型の名前の形式. 0001.sql の link_types の CHECK と同じ.
var linkTypeName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func (t LinkType) validate() error {
	if !linkTypeName.MatchString(t.Name) {
		return fmt.Errorf("%w: リンクの型の名前 %q は, 英小文字で始まり英小文字, 数字, _ だけからなる 64 バイト以下である必要があります", ErrInvalid, t.Name)
	}
	if t.Major < 0 {
		return fmt.Errorf("%w: リンクの型 %s の major 番号が負です", ErrInvalid, t.Name)
	}
	return nil
}

// LinkTypeDefinition は, リンクの型の定義. この版の Lyell が知らない型のリンクは, 別のマシンから受け取った時のように, 定義と一緒の時だけ書ける.
type LinkTypeDefinition struct {
	LinkType
	Minor, Patch int
	// KeepsTarget は, リンクがある間はリンク先を削除しないか.
	KeepsTarget bool
	// Copyable は, 新しい revision を作る時に, 元の revision のリンクを引き継いでよいか.
	Copyable bool
	// UnknownFields は, この版の Lyell が知らない定義の項目のバイト列. そのまま保存する.
	UnknownFields []byte
}

// LinkTarget は, リンク先. Revision (特定の revision) と Item (その時点の head) のどちらか一方だけを指定する. リンク先はこのマシンに無くてもよい.
type LinkTarget struct {
	Revision ID
	Item     ID
}

// Link は, revision から親以外へのリンク. 親は Revision.Parents と NewRevision.Parents で扱う.
type Link struct {
	Type   LinkType
	Target LinkTarget
}

func compareLinks(a, b Link) int {
	return cmp.Or(
		cmp.Compare(a.Type.Name, b.Type.Name),
		cmp.Compare(a.Type.Major, b.Type.Major),
		bytes.Compare(a.Target.Revision[:], b.Target.Revision[:]),
		bytes.Compare(a.Target.Item[:], b.Target.Item[:]),
	)
}

func (t LinkTarget) validate() error {
	if (t.Revision == ID{}) == (t.Item == ID{}) {
		return fmt.Errorf("%w: リンク先には revision と item のどちらか一方だけを指定する必要があります", ErrInvalid)
	}
	return nil
}

// validateLinks は, 書き込むリンクと一緒に渡された型の定義を DB を参照せずに検証し, リンクを型とリンク先の順に並べて返す. 知らない型のリンクは, defs に定義がある時だけ受け付ける.
func validateLinks(links []Link, defs []LinkTypeDefinition) ([]Link, error) {
	defined := make(map[LinkType]bool, len(defs))
	for _, d := range defs {
		if err := d.validate(); err != nil {
			return nil, err
		}
		if d.Minor < 0 || d.Patch < 0 {
			return nil, fmt.Errorf("%w: リンクの型 %s の版 %d.%d.%d に負の番号があります", ErrInvalid, d.Name, d.Major, d.Minor, d.Patch)
		}
		if defined[d.LinkType] {
			return nil, fmt.Errorf("%w: リンクの型 %s (major %d) の定義が重複しています", ErrInvalid, d.Name, d.Major)
		}
		defined[d.LinkType] = true
	}
	sorted := slices.Clone(links)
	slices.SortFunc(sorted, compareLinks)
	for i, l := range sorted {
		if err := l.Type.validate(); err != nil {
			return nil, err
		}
		if l.Type == LinkParent {
			return nil, fmt.Errorf("%w: 親は Links ではなく Parents で指定します", ErrInvalid)
		}
		if !l.Type.known() && !defined[l.Type] {
			return nil, fmt.Errorf("%w: リンクの型 %s (major %d) はこの版の Lyell が知らない型で, 定義が渡されていません", ErrInvalid, l.Type.Name, l.Type.Major)
		}
		if err := l.Target.validate(); err != nil {
			return nil, err
		}
		if i > 0 && compareLinks(sorted[i-1], l) == 0 {
			return nil, fmt.Errorf("%w: 同じリンク先への型 %s のリンクが重複しています", ErrInvalid, l.Type.Name)
		}
	}
	return sorted, nil
}

// registerLinkTypes は, 渡された型の定義を tx で登録する. 同じ名前と major 番号の定義があれば, 渡された定義の方が新しい時だけ置き換える (知らない項目も新しい方にする). フラグが食い違う定義は拒否する. write が false なら検証だけを行う.
func registerLinkTypes(ctx context.Context, tx *sql.Tx, defs []LinkTypeDefinition, write bool) error {
	for _, d := range defs {
		var minor, patch int
		var keeps, copyable bool
		err := tx.QueryRowContext(ctx, "SELECT minor, patch, keeps_target, copyable FROM link_types WHERE name = ? AND major = ?", d.Name, d.Major).
			Scan(&minor, &patch, &keeps, &copyable)
		if errors.Is(err, sql.ErrNoRows) {
			if write {
				if _, err := tx.ExecContext(ctx, "INSERT INTO link_types (name, major, minor, patch, keeps_target, copyable, unknown_fields) VALUES (?, ?, ?, ?, ?, ?, ?)",
					d.Name, d.Major, d.Minor, d.Patch, d.KeepsTarget, d.Copyable, d.UnknownFields); err != nil {
					return err
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		// 同じ major 番号の中ではリンクの扱いは変わらないので, フラグが違う定義は同じ型の定義ではない.
		if keeps != d.KeepsTarget || copyable != d.Copyable {
			return fmt.Errorf("%w: リンクの型 %s (major %d) の定義のフラグが, 登録済みの定義と食い違います", ErrInvalid, d.Name, d.Major)
		}
		if write && cmp.Or(cmp.Compare(d.Minor, minor), cmp.Compare(d.Patch, patch)) > 0 {
			if _, err := tx.ExecContext(ctx, "UPDATE link_types SET minor = ?, patch = ?, unknown_fields = ? WHERE name = ? AND major = ?",
				d.Minor, d.Patch, d.UnknownFields, d.Name, d.Major); err != nil {
				return err
			}
		}
	}
	return nil
}

// insertLinks は, revision id からのリンクを記録する. この版は未知のフィールドの列を NULL で書く.
func insertLinks(ctx context.Context, tx *sql.Tx, id ID, links []Link) error {
	for _, l := range links {
		if _, err := tx.ExecContext(ctx, "INSERT INTO revision_links (source_revision, target_revision, target_item, type_name, type_major) VALUES (?, ?, ?, ?, ?)",
			id[:], idColumn(l.Target.Revision), idColumn(l.Target.Item), l.Type.Name, l.Type.Major); err != nil {
			return fmt.Errorf("リンク (型 %s) を記録できませんでした: %w", l.Type.Name, err)
		}
	}
	return nil
}

// idColumn は, ゼロ値の ID を NULL として書くための値を返す.
func idColumn(id ID) any {
	if id == (ID{}) {
		return nil
	}
	return id[:]
}

// readLinks は, revision id からの親以外のリンクを, 型とリンク先の順に返す.
func readLinks(ctx context.Context, tx *sql.Tx, id ID) ([]Link, error) {
	rows, err := tx.QueryContext(ctx, `SELECT type_name, type_major, target_revision, target_item FROM revision_links
		WHERE source_revision = ? AND NOT (type_name = ? AND type_major = ?)`, id[:], LinkParent.Name, LinkParent.Major)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var links []Link
	for rows.Next() {
		var l Link
		var rev, item []byte
		if err := rows.Scan(&l.Type.Name, &l.Type.Major, &rev, &item); err != nil {
			return nil, err
		}
		if rev != nil {
			if err := scanID(rev, &l.Target.Revision); err != nil {
				return nil, err
			}
		}
		if item != nil {
			if err := scanID(item, &l.Target.Item); err != nil {
				return nil, err
			}
		}
		links = append(links, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 並びは ValidateCommitRevision が返す Links と同じにする (compareLinks).
	slices.SortFunc(links, compareLinks)
	return links, nil
}

// LinksTo は, 型 t で target を指すリンク元の revision を ID 順に返す. 親 (LinkParent) を指定すると, revision の子を返す. リンク先がこのマシンに無くても探せる.
func (s *Store) LinksTo(ctx context.Context, target LinkTarget, t LinkType) ([]ID, error) {
	if err := target.validate(); err != nil {
		return nil, err
	}
	column, id := "target_revision", target.Revision
	if target.Item != (ID{}) {
		column, id = "target_item", target.Item
	}
	// revision_links_target_revision と revision_links_target_item のインデックスで, リンク元の順に読む.
	rows, err := s.read.QueryContext(ctx, "SELECT source_revision FROM revision_links WHERE "+column+" = ? AND type_name = ? AND type_major = ? ORDER BY source_revision", id[:], t.Name, t.Major)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var sources []ID
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var source ID
		if err := scanID(raw, &source); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// ErrUnchanged は, 統合でない CommitRevision の revision が, 前提の head (From) と blob, body, 親以外のリンクの全てで同じ時のエラー. errors.Is(err, ErrInvalid) も真になる.
var ErrUnchanged = fmt.Errorf("%w: 前提の head と変わらない revision は記録できません", ErrInvalid)

// checkChanged は, 統合でない revision r が From から何か変えているかを tx で検証する. From が間引かれていれば比べられないので, 変わったものとして扱う. links は validateLinks で並べたもの.
func checkChanged(ctx context.Context, tx *sql.Tx, r NewRevision, links []Link) error {
	if len(r.Parents) > 1 {
		return nil
	}
	var blob, body []byte
	var thinned bool
	if err := tx.QueryRowContext(ctx, "SELECT blob_sha256, body, body IS NULL FROM revisions WHERE id = ?", r.From[:]).Scan(&blob, &body, &thinned); err != nil {
		return err
	}
	if thinned {
		return nil
	}
	var newBlob []byte
	if r.Content.hasBlob() {
		newBlob = r.Content.Blob[:]
	}
	if !bytes.Equal(blob, newBlob) {
		return nil
	}
	same, err := sameBodyContent(body, r.Content.Body)
	if err != nil || !same {
		return err
	}
	prev, err := readLinks(ctx, tx, r.From)
	if err != nil {
		return err
	}
	if !slices.Equal(prev, links) {
		return nil
	}
	return ErrUnchanged
}
