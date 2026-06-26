package main

import (
	"path/filepath"
	"testing"
)

func TestScanModels(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "app", "Http", "Models"))
	mkdir(t, filepath.Join(dir, "app", "Modules", "Billing", "Models"))

	// Braceless namespace, $-sigil properties, chained relation.
	write(t, filepath.Join(dir, "app", "Http", "Models", "User.php"), `<?php
namespace App\Http\Models;
use Illuminate\Foundation\Auth\User as Authenticatable;
class User extends Authenticatable
{
    protected $table = 'users';
    protected $fillable = ['name', 'email'];
    protected $casts = ['verified_at' => 'datetime', 'active' => 'bool'];
    public function posts()
    {
        return $this->hasMany(Post::class)->latest();
    }
}
`)
	// Modular app, different folder, belongsTo.
	write(t, filepath.Join(dir, "app", "Modules", "Billing", "Models", "Invoice.php"), `<?php
namespace App\Modules\Billing\Models;
use App\Http\Models\BaseModel;
class Invoice extends BaseModel
{
    protected $table = 'invoices';
    public function customer()
    {
        return $this->belongsTo(\App\Http\Models\Customer::class, 'customer_id');
    }
}
`)

	p := &Project{Root: dir}
	found := p.scanModels()
	got := map[string]modelInfo{}
	for _, m := range found {
		got[m.Name] = m
	}

	u, ok := got["User"]
	if !ok {
		t.Fatalf("User model not found; found %d models", len(found))
	}
	if u.Class != "App\\Http\\Models\\User" {
		t.Errorf("User class = %q", u.Class)
	}
	if u.Table != "users" {
		t.Errorf("User table = %q", u.Table)
	}
	if len(u.Fillable) != 2 || u.Fillable[0] != "name" {
		t.Errorf("User fillable = %v", u.Fillable)
	}
	if u.Casts["verified_at"] != "datetime" || u.Casts["active"] != "bool" {
		t.Errorf("User casts = %v", u.Casts)
	}
	if len(u.Relations) != 1 || u.Relations[0].Type != "hasMany" || u.Relations[0].Method != "posts" {
		t.Errorf("User relations = %v", u.Relations)
	}

	inv, ok := got["Invoice"]
	if !ok {
		t.Fatal("Invoice model (modular) not found")
	}
	if inv.Class != "App\\Modules\\Billing\\Models\\Invoice" {
		t.Errorf("Invoice class = %q", inv.Class)
	}
	if len(inv.Relations) != 1 || inv.Relations[0].Type != "belongsTo" {
		t.Errorf("Invoice relations = %v", inv.Relations)
	}
}
