package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/safe/common"
)

type ExternalAsset struct {
	AssetId      string
	ChainId      string
	Name         string
	Symbol       string
	PriceUSD     string
	Uri          sql.NullString
	IconUrl      sql.NullString
	CreatedAt    time.Time
	DeployedHash sql.NullString
}

var externalAssetCols = []string{"asset_id", "chain_id", "name", "symbol", "price_usd", "uri", "icon_url", "deployed_hash", "created_at"}

func externalAssetFromRow(row Row) (*ExternalAsset, error) {
	var a ExternalAsset
	err := row.Scan(&a.AssetId, &a.ChainId, &a.Name, &a.Symbol, &a.PriceUSD, &a.Uri, &a.IconUrl, &a.DeployedHash, &a.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

func (s *SQLite3Store) WriteExternalAssets(ctx context.Context, assets []*ExternalAsset) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	for _, asset := range assets {
		vals := []any{asset.AssetId, asset.ChainId, asset.Name, asset.Symbol, asset.PriceUSD, nil, nil, nil, asset.CreatedAt}
		err = s.execOne(ctx, tx, buildInsertionSQL("external_assets", externalAssetCols), vals...)
		if err != nil {
			return fmt.Errorf("INSERT external_assets %v", err)
		}
	}

	return tx.Commit()
}

func (s *SQLite3Store) UpdateExternalAssetUri(ctx context.Context, id, uri string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	query := "UPDATE external_assets SET uri=? WHERE asset_id=? AND uri IS NULL"
	_, err = tx.ExecContext(ctx, query, uri, id)
	if err != nil {
		return fmt.Errorf("SQLite3Store UPDATE external_assets %v", err)
	}

	return tx.Commit()
}

func (s *SQLite3Store) UpdateExternalAssetIconUrl(ctx context.Context, id, uri string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	query := "UPDATE external_assets SET icon_url=? WHERE asset_id=? AND icon_url IS NULL"
	_, err = tx.ExecContext(ctx, query, uri, id)
	if err != nil {
		return fmt.Errorf("SQLite3Store UPDATE external_assets %v", err)
	}

	return tx.Commit()
}

func (s *SQLite3Store) MarkExternalAssetDeployed(ctx context.Context, assets []*solanaApp.DeployedAsset, hash string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	for _, a := range assets {
		query := "UPDATE external_assets SET deployed_hash=? WHERE asset_id=? AND deployed_hash IS NULL"
		err = s.execOne(ctx, tx, query, hash, a.AssetId)
		if err != nil {
			return fmt.Errorf("SQLite3Store UPDATE external_assets %v", err)
		}
	}

	return tx.Commit()
}

func (s *SQLite3Store) UpdateExternalAssetsInfo(ctx context.Context, assets []*bot.Asset) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	for _, a := range assets {
		query := "UPDATE external_assets SET chain_id=?, name=?, symbol=?, price_usd=? WHERE asset_id=?"
		err = s.execOne(ctx, tx, query, a.ChainID, a.DisplayName, a.DisplaySymbol, a.PriceUSD, a.AssetID)
		if err != nil {
			return fmt.Errorf("SQLite3Store UPDATE external_assets %v", err)
		}
	}

	return tx.Commit()
}

func (s *SQLite3Store) ReadExternalAsset(ctx context.Context, id string) (*ExternalAsset, error) {
	query := fmt.Sprintf("SELECT %s FROM external_assets WHERE asset_id=?", strings.Join(externalAssetCols, ","))
	row := s.db.QueryRowContext(ctx, query, id)

	return externalAssetFromRow(row)
}

func (s *SQLite3Store) ListUndeployedAssets(ctx context.Context) ([]*ExternalAsset, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	query := fmt.Sprintf("SELECT %s FROM external_assets WHERE deployed_hash IS NULL LIMIT 500", strings.Join(externalAssetCols, ","))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var as []*ExternalAsset
	for rows.Next() {
		asset, err := externalAssetFromRow(rows)
		if err != nil {
			return nil, err
		}
		as = append(as, asset)
	}
	return as, nil
}

func (s *SQLite3Store) DeployedExternalAssetMap(ctx context.Context) (map[string]*ExternalAsset, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	query := fmt.Sprintf("SELECT %s FROM external_assets WHERE icon_url IS NOT NULL AND deployed_hash IS NOT NULL LIMIT 500", strings.Join(externalAssetCols, ","))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	am := make(map[string]*ExternalAsset)
	for rows.Next() {
		asset, err := externalAssetFromRow(rows)
		if err != nil {
			return nil, err
		}
		am[asset.AssetId] = asset
	}
	return am, nil
}

func (s *SQLite3Store) ListExternalAssetIds(ctx context.Context) ([]string, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	query := fmt.Sprintf("SELECT %s FROM external_assets LIMIT 500", strings.Join(externalAssetCols, ","))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		asset, err := externalAssetFromRow(rows)
		if err != nil {
			return nil, err
		}
		ids = append(ids, asset.AssetId)
	}
	return ids, nil
}
