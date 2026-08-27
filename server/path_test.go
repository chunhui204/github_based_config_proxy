package server

import "testing"

func TestIdentityFromGitHubPath(t *testing.T) {
	tests := []struct {
		name      string
		rootPath  string
		filePath  string
		want      ConfigIdentity
		wantFound bool
	}{
		{
			name:      "normal path",
			rootPath:  "configs",
			filePath:  "configs/payment/risk.yaml",
			want:      ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"},
			wantFound: true,
		},
		{
			name:      "root with slash",
			rootPath:  "/configs/",
			filePath:  "/configs/payment/risk/limit.yaml",
			want:      ConfigIdentity{Namespace: "payment/risk", ConfigKey: "limit.yaml"},
			wantFound: true,
		},
		{
			name:      "file without namespace",
			rootPath:  "configs",
			filePath:  "configs/app.yaml",
			wantFound: false,
		},
		{
			name:      "outside root",
			rootPath:  "configs",
			filePath:  "other/payment/risk.yaml",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := IdentityFromGitHubPath(tt.rootPath, tt.filePath)
			if found != tt.wantFound {
				t.Fatalf("found=%v, want %v", found, tt.wantFound)
			}
			if got != tt.want {
				t.Fatalf("identity=%+v, want %+v", got, tt.want)
			}
		})
	}
}
