package app

import (
	"path/filepath"
	"testing"
)

func TestScanModels(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "app", "Http", "Models"))
	mkdir(t, filepath.Join(dir, "app", "Modules", "Billing", "Models"))

	writeUserAndInvoiceModels(t, dir)

	checkUserModel(t, mustFindModel(t, scanModelsByName(dir), "User"))

	// Laravel 11 casts() method form.
	mkdir(t, filepath.Join(dir, "app", "Models"))
	write(t, filepath.Join(dir, "app", "Models", "Payment.php"), `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
class Payment extends Model
{
    protected $table = 'payments';
    protected function casts(): array
    {
        return ['amount' => 'integer', 'paid_at' => 'datetime'];
    }
}
`)

	got := scanModelsByName(dir)

	pay := mustFindModel(t, got, "Payment")
	if pay.Casts["amount"] != "integer" || pay.Casts["paid_at"] != "datetime" {
		t.Errorf("Payment casts() method not extracted: %v", pay.Casts)
	}

	inv := mustFindModel(t, got, "Invoice")
	if inv.Class != "App\\Modules\\Billing\\Models\\Invoice" {
		t.Errorf("Invoice class = %q", inv.Class)
	}

	if len(inv.Relations) != 1 || inv.Relations[0].Type != "belongsTo" {
		t.Errorf("Invoice relations = %v", inv.Relations)
	}
}

// writeUserAndInvoiceModels writes a braceless-namespace User with $-sigil
// properties and a chained relation, and a modular-app Invoice with belongsTo.
func writeUserAndInvoiceModels(t *testing.T, dir string) {
	t.Helper()

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
}

// scanModelsByName scans a fresh project at dir and indexes models by name.
func scanModelsByName(dir string) map[string]modelInfo {
	got := map[string]modelInfo{}
	for _, info := range newProject(dir).scanModels() {
		got[info.Name] = info
	}

	return got
}

func mustFindModel(t *testing.T, got map[string]modelInfo, name string) modelInfo {
	t.Helper()

	info, found := got[name]
	if !found {
		t.Fatalf("%s model not found; found %d models", name, len(got))
	}

	return info
}

func checkUserModel(t *testing.T, user modelInfo) {
	t.Helper()

	if user.Class != "App\\Http\\Models\\User" {
		t.Errorf("User class = %q", user.Class)
	}

	if user.Table != "users" {
		t.Errorf("User table = %q", user.Table)
	}

	if len(user.Fillable) != 2 || user.Fillable[0] != "name" {
		t.Errorf("User fillable = %v", user.Fillable)
	}

	if user.Casts["verified_at"] != "datetime" || user.Casts["active"] != "bool" {
		t.Errorf("User casts = %v", user.Casts)
	}

	if len(user.Relations) != 1 || user.Relations[0].Type != "hasMany" || user.Relations[0].Method != "posts" {
		t.Errorf("User relations = %v", user.Relations)
	}
}

// A model using syntax past the parser's 8.1 grammar is still listed, flagged
// partial so the client knows the extracted fields may be incomplete.
func TestScanModelsNewerSyntaxIsPartial(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "app", "Models"))
	write(t, filepath.Join(dir, "app", "Models", "Order.php"), `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
class Order extends Model
{
    protected $table = 'orders';
}
`)
	write(t, filepath.Join(dir, "app", "Models", "Product.php"), `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
class Product extends Model
{
    protected $table = 'products';
    public string $label { get => strtoupper($this->name); }
}
`)

	got := scanModelsByName(dir)

	if order, found := got["Order"]; !found || order.Partial {
		t.Errorf("Order = %+v (found=%v), want found and not partial", order, found)
	}

	product, found := got["Product"]
	if !found {
		t.Fatal("Product model (property hook) not found")
	}

	if !product.Partial {
		t.Error("Product.Partial = false, want true")
	}

	if product.Class != "App\\Models\\Product" || product.Table != "products" {
		t.Errorf("Product class = %q, table = %q", product.Class, product.Table)
	}
}
