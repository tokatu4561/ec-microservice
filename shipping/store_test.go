package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func uniqueID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func fixture(t *testing.T) (postgresStore, context.Context) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("run shipping-test for DB tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err = pool.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	return postgresStore{pool}, ctx
}
func TestShipmentLifecyclePostgres(t *testing.T) {
	s, ctx := fixture(t)
	for _, mode := range []string{"success", "fail"} {
		in := ShipmentInput{uniqueID(), []ShipmentItem{{2, 1}, {1, 2}}, mode}
		p, err := s.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		want := "requested"
		if mode == "fail" {
			want = "failed"
		}
		if p.Status != want || p.Items[0].ProductID != 1 || p.CreatedAt.IsZero() {
			t.Fatal(p)
		}
		in.Items = []ShipmentItem{{1, 2}, {2, 1}}
		replay, err := s.Create(ctx, in)
		if err != nil || !reflect.DeepEqual(p, replay) {
			t.Fatalf("replay %+v %v", replay, err)
		}
		other := in
		other.Items = []ShipmentItem{{1, 3}, {2, 1}}
		if _, err = s.Create(ctx, other); !errors.Is(err, errConflict) {
			t.Fatal(err)
		}
		other = in
		other.Mode = "fail"
		if mode == "fail" {
			other.Mode = "success"
		}
		if _, err = s.Create(ctx, other); !errors.Is(err, errConflict) {
			t.Fatal(err)
		}
		if mode == "fail" {
			if _, err = s.Cancel(ctx, p.OrderID); !errors.Is(err, errConflict) {
				t.Fatal(err)
			}
		} else {
			c, err := s.Cancel(ctx, p.OrderID)
			if err != nil || c.Status != "cancelled" {
				t.Fatal(c, err)
			}
			again, err := s.Cancel(ctx, p.OrderID)
			if err != nil || !reflect.DeepEqual(c, again) {
				t.Fatal(again, err)
			}
			replay, err = s.Create(ctx, in)
			if err != nil || !reflect.DeepEqual(c, replay) {
				t.Fatal("resurrected", replay, err)
			}
		}
		before, err := s.Get(ctx, p.OrderID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(ctx, schema); err != nil {
			t.Fatal(err)
		}
		after, err := s.Get(ctx, p.OrderID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal(after, err)
		}
	}
	if _, err := s.Cancel(ctx, uniqueID()); !errors.Is(err, errNotFound) {
		t.Fatal(err)
	}
}
func TestConcurrentCreateCancelPostgres(t *testing.T) {
	s, ctx := fixture(t)
	in := ShipmentInput{uniqueID(), []ShipmentItem{{1, 2}}, "success"}
	run := func(op func() (Shipment, error)) []Shipment {
		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make(chan Shipment, 20)
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				p, e := op()
				if e != nil {
					t.Error(e)
				}
				results <- p
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		var ps []Shipment
		for p := range results {
			ps = append(ps, p)
		}
		return ps
	}
	for _, op := range []func() (Shipment, error){func() (Shipment, error) { return s.Create(ctx, in) }, func() (Shipment, error) { return s.Cancel(ctx, in.OrderID) }} {
		ps := run(op)
		for _, p := range ps {
			if !reflect.DeepEqual(ps[0], p) {
				t.Fatal("different records", ps)
			}
		}
	}
	// Creation racing with repeated cancellation must never resurrect the accepted request.
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, e := s.Create(ctx, in)
			if e != nil {
				t.Error(e)
			}
		}()
		go func() {
			defer wg.Done()
			_, e := s.Cancel(ctx, in.OrderID)
			if e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	p, err := s.Get(ctx, in.OrderID)
	if err != nil || p.Status != "cancelled" {
		t.Fatal(p, err)
	}
	var n int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM shipments WHERE order_id=$1`, in.OrderID).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
