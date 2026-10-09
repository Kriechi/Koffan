package handlers

import (
	"encoding/json"
	"os"
	"reflect"
	"shopping-list/db"
	"testing"
)

func TestCompletionOrderMatchesClientFixtures(t *testing.T) {
	initTestDatabase(t)
	var fixtures struct {
		Items []struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			SortOrder int    `json:"sort_order"`
		} `json:"items"`
		Orders map[string][]int `json:"orders"`
	}
	data, err := os.ReadFile("../test/item-order-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	list, err := db.CreateList(db.DefaultWorkspaceID, "Ordering", "cart")
	if err != nil {
		t.Fatal(err)
	}
	section, err := db.CreateSectionForList(list.ID, "Food")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]int{}
	for _, fixture := range fixtures.Items {
		item, err := db.CreateItem(section.ID, fixture.Name, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		ids[item.ID] = fixture.ID
		if _, err := db.DB.Exec("UPDATE items SET sort_order = ? WHERE id = ?", fixture.SortOrder, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	for mode, expected := range fixtures.Orders {
		t.Run(mode, func(t *testing.T) {
			if _, err := db.UpdateSectionSortMode(section.ID, mode); err != nil {
				t.Fatal(err)
			}
			for _, completed := range []bool{true, false} {
				for id := range ids {
					if _, _, err := db.SetItemCompleted(id, completed); err != nil {
						t.Fatal(err)
					}
				}
				items, err := db.GetItemsBySection(section.ID)
				if err != nil {
					t.Fatal(err)
				}
				actual := make([]int, 0, len(items))
				for _, item := range items {
					actual = append(actual, ids[item.ID])
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Fatalf("completed=%v: got %v, want %v", completed, actual, expected)
				}
			}
		})
	}
}
