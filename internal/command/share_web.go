// Copyright (c) 2020 tickstep.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package command

import (
	"encoding/csv"
	"fmt"
	"github.com/tickstep/aliyunpan-api/aliyunpan"
	"github.com/tickstep/aliyunpan-api/aliyunpan/apierror"
	"github.com/tickstep/aliyunpan/cmder"
	"github.com/tickstep/aliyunpan/cmder/cmdtable"
	"github.com/tickstep/aliyunpan/internal/config"
	"github.com/urfave/cli"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"
)

// web端分享接口
func CmdShareWeb() cli.Command {
	return cli.Command{
		Name:      "sharew",
		Usage:     "分享文件/目录",
		UsageText: cmder.App().Name + " sharew",
		Category:  "阿里云盘",
		Before:    ReloadConfigFunc,
		Action: func(c *cli.Context) error {
			cli.ShowCommandHelp(c, c.Command.Name)
			return nil
		},
		Subcommands: []cli.Command{
			{
				Name:      "set",
				Aliases:   []string{"s"},
				Usage:     "设置分享文件/目录",
				UsageText: cmder.App().Name + " share set <文件/目录1> <文件/目录2> ...",
				Description: `
示例:

    创建文件 1.mp4 的分享链接 
	aliyunpan share set -mode 1 1.mp4

    创建 /我的视频/ 目录下所有mp4文件的分享链接，支持通配符
	aliyunpan share set -mode 1 /我的视频/*.mp4

    创建文件 1.mp4 的分享链接，并指定分享密码为2333
	aliyunpan share set -mode 1 -sharePwd 2333 1.mp4

    创建文件 1.mp4 的分享链接，并指定有效期为1天
	aliyunpan share set -mode 1 -time 1 1.mp4

    创建文件 1.mp4 的快传链接
	aliyunpan share set 1.mp4
`,
				Action: func(c *cli.Context) error {
					if c.NArg() < 1 {
						cli.ShowCommandHelp(c, c.Command.Name)
						return nil
					}
					if config.Config.ActiveUser() == nil {
						fmt.Println("未登录账号")
						return nil
					}
					if config.Config.ActiveUser().PanClient().WebapiPanClient() == nil {
						fmt.Println("WEB客户端未登录，请登录后再使用该命令")
						return nil
					}
					et := ""
					timeFlag := "0"
					if c.IsSet("time") {
						timeFlag = c.String("time")
					}
					now := time.Now()
					if timeFlag == "1" {
						et = now.Add(time.Duration(1) * time.Hour * 24).Format("2006-01-02 15:04:05")
					} else if timeFlag == "2" {
						et = now.Add(time.Duration(7) * time.Hour * 24).Format("2006-01-02 15:04:05")
					} else {
						et = ""
					}

					sharePwd := ""
					if c.IsSet("sharePwd") {
						sharePwd = c.String("sharePwd")
					}

					modeFlag := "3"
					if c.IsSet("mode") {
						modeFlag = c.String("mode")
					}
					if modeFlag == "1" || modeFlag == "2" {
						if config.Config.ActiveUser().ActiveDriveId != config.Config.ActiveUser().DriveList.GetResourceDriveId() {
							// 只有资源库才支持私有、公开分享
							fmt.Println("只有资源库才支持分享链接，其他请使用快传链接")
							return nil
						}
					}

					if modeFlag == "1" {
						if sharePwd == "" {
							sharePwd = RandomStr(4)
						}
					} else {
						sharePwd = ""
					}
					RunShareSet(modeFlag, parseDriveId(c), c.Args(), et, sharePwd)
					return nil
				},
				Flags: []cli.Flag{
					cli.StringFlag{
						Name:  "driveId",
						Usage: "网盘ID",
						Value: "",
					},
					cli.StringFlag{
						Name:  "time",
						Usage: "有效期，0-永久，1-1天，2-7天",
						Value: "0",
					},
					cli.StringFlag{
						Name:  "mode",
						Usage: "模式，1-私密分享，2-公开分享，3-快传",
						Value: "3",
					},
					cli.StringFlag{
						Name:  "sharePwd",
						Usage: "自定义私密分享密码，4个字符，没有指定则随机生成",
						Value: "",
					},
				},
			},
			{
				Name:      "list",
				Aliases:   []string{"l"},
				Usage:     "列出已分享文件/目录",
				UsageText: cmder.App().Name + " share list",
				Action: func(c *cli.Context) error {
					if config.Config.ActiveUser() == nil {
						fmt.Println("未登录账号")
						return nil
					}
					if config.Config.ActiveUser().PanClient().WebapiPanClient() == nil {
						fmt.Println("WEB客户端未登录，请登录后再使用该命令")
						return nil
					}
					RunShareList()
					return nil
				},
				Flags: []cli.Flag{},
			},
			{
				Name:        "cancel",
				Aliases:     []string{"c"},
				Usage:       "取消分享文件/目录",
				UsageText:   cmder.App().Name + " share cancel <shareid_1> <shareid_2> ...",
				Description: `目前只支持通过分享id (shareid) 来取消分享.`,
				Action: func(c *cli.Context) error {
					if config.Config.ActiveUser() == nil {
						fmt.Println("未登录账号")
						return nil
					}
					if config.Config.ActiveUser().PanClient().WebapiPanClient() == nil {
						fmt.Println("WEB客户端未登录，请登录后再使用该命令")
						return nil
					}
					if c.NArg() < 1 {
						cli.ShowCommandHelp(c, c.Command.Name)
						return nil
					}
					RunShareCancel(c.Args())
					return nil
				},
			},
			{
				Name:      "export",
				Usage:     "导出分享记录保存到文件",
				UsageText: cmder.App().Name + " share export <csv file path>",
				Description: `
导出分享记录，并保存到指定的文件。目前支持csv格式
  
示例:
    导出所有有效的分享并保存成文件
	aliyunpan share export "d:\myfoler\share_list.csv"

    导出所有的分享并保存成文件
	aliyunpan share export -option 2 "d:\myfoler\share_list.csv"
`,
				Action: func(c *cli.Context) error {
					if config.Config.ActiveUser() == nil {
						fmt.Println("未登录账号")
						return nil
					}
					if config.Config.ActiveUser().PanClient().WebapiPanClient() == nil {
						fmt.Println("WEB客户端未登录，请登录后再使用该命令")
						return nil
					}
					if c.NArg() < 1 {
						cli.ShowCommandHelp(c, c.Command.Name)
						return nil
					}
					opt := c.String("option")
					if opt == "" {
						opt = "1"
					}
					filePath := c.Args()[0]
					RunShareExport(opt, filePath)
					return nil
				},
				Flags: []cli.Flag{
					cli.StringFlag{
						Name:  "option",
						Usage: "导出选项，1-有效分享 2-全部分享",
						Value: "1",
					},
				},
			},
		},
	}
}

// RunShareSet 执行分享
func RunShareSet(modeFlag, driveId string, paths []string, expiredTime string, sharePwd string) {
	if len(paths) <= 0 {
		fmt.Println("请指定文件路径")
		return
	}
	activeUser := GetActiveUser()
	panClient := activeUser.PanClient()

	allFileList := []*aliyunpan.FileEntity{}
	for idx := 0; idx < len(paths); idx++ {
		absolutePath := path.Clean(activeUser.PathJoin(driveId, paths[idx]))
		fileList, err1 := matchPathByShellPattern(driveId, absolutePath)
		if err1 != nil {
			fmt.Println("文件不存在: " + absolutePath)
			continue
		}
		if fileList == nil || len(fileList) == 0 {
			// 文件不存在
			fmt.Println("文件不存在: " + absolutePath)
			continue
		}
		// 匹配的文件
		allFileList = append(allFileList, fileList...)
	}

	fidList := []string{}
	for _, f := range allFileList {
		fidList = append(fidList, f.FileId)
	}

	if len(fidList) == 0 {
		fmt.Printf("没有指定有效的文件\n")
		return
	}

	if modeFlag == "3" {
		// 快传
		r, err1 := panClient.WebapiPanClient().FastShareLinkCreate(aliyunpan.FastShareCreateParam{
			DriveId:    driveId,
			FileIdList: fidList,
		})
		if err1 != nil || r == nil {
			if err1.Code == apierror.ApiCodeFileShareNotAllowed {
				fmt.Printf("创建快传链接失败: 该文件类型不允许分享\n")
			} else {
				fmt.Printf("创建快传链接失败: %s\n", err1)
			}
			return
		}

		fmt.Printf("创建快传链接成功\n")
		fmt.Printf("链接：%s\n", r.ShareUrl)
	} else {
		// 分享
		r, err1 := panClient.WebapiPanClient().ShareLinkCreate(aliyunpan.ShareCreateParam{
			DriveId:    driveId,
			SharePwd:   sharePwd,
			Expiration: expiredTime,
			FileIdList: fidList,
		})
		if err1 != nil || r == nil {
			if err1.Code == apierror.ApiCodeFileShareNotAllowed {
				fmt.Printf("创建分享链接失败: 该文件类型不允许分享\n")
			} else {
				fmt.Printf("创建分享链接失败: %s\n", err1)
			}
			return
		}

		fmt.Printf("创建分享链接成功\n")
		if len(sharePwd) > 0 {
			fmt.Printf("链接：%s 提取码：%s\n", r.ShareUrl, r.SharePwd)
		} else {
			fmt.Printf("链接：%s\n", r.ShareUrl)
		}
	}
}

// RunShareList 执行列出分享列表
func RunShareList() {
	activeUser := GetActiveUser()
	records, err := activeUser.PanClient().WebapiPanClient().ShareLinkList(activeUser.UserId)
	if err != nil {
		fmt.Printf("获取分享列表失败: %s\n", err)
		return
	}

	tb := cmdtable.NewTable(os.Stdout)
	tb.SetHeader([]string{"#", "ShARE_ID", "分享链接", "提取码", "文件名", "过期时间", "状态"})
	now := time.Now()
	for k, record := range records {
		et := "永久有效"
		if len(record.Expiration) > 0 {
			et = record.Expiration
		}
		status := "有效"
		if record.Status == "enabled" {
			if record.FirstFile == nil {
				status = "已删除"
			} else {
				cz := time.FixedZone("CST", 8*3600)
				if len(record.Expiration) > 0 {
					expiredTime, _ := time.ParseInLocation("2006-01-02 15:04:05", record.Expiration, cz)
					if expiredTime.Unix() < now.Unix() {
						status = "已过期"
					}
				}
			}
		} else if record.Status == "forbidden" {
			status = "违规"
		}

		tb.Append([]string{strconv.Itoa(k + 1), record.ShareId, record.ShareUrl, record.SharePwd,
			record.ShareName,
			//record.FileIdList[0],
			et,
			status})
	}
	tb.Render()
}

// RunShareCancel 执行取消分享
func RunShareCancel(shareIdList []string) {
	if len(shareIdList) == 0 {
		fmt.Printf("取消分享操作失败, 没有任何 shareid\n")
		return
	}

	activeUser := GetActiveUser()
	r, err := activeUser.PanClient().WebapiPanClient().ShareLinkCancel(shareIdList)
	if err != nil {
		fmt.Printf("取消分享操作失败: %s\n", err)
		return
	}

	if r != nil && len(r) > 0 {
		fmt.Printf("取消分享操作成功\n")
	} else {
		fmt.Printf("取消分享操作失败\n")
	}
}

func RunShareExport(option, saveFilePath string) {
	activeUser := GetActiveUser()
	records, err := activeUser.PanClient().WebapiPanClient().ShareLinkList(activeUser.UserId)
	if err != nil {
		fmt.Printf("获取分享列表失败: %s\n", err)
		return
	}

	columns := [][]string{{"序号", "分享ID", "分享链接", "提取码", "文件名", "过期时间", "状态"}}
	now := time.Now()
	idx := 1
	for _, record := range records {
		et := "永久有效"
		if len(record.Expiration) > 0 {
			et = record.Expiration
		}
		status := "有效"
		if record.FirstFile == nil {
			status = "已删除"
			if option == "1" {
				continue
			}
		} else {
			cz := time.FixedZone("CST", 8*3600)
			if len(record.Expiration) > 0 {
				expiredTime, _ := time.ParseInLocation("2006-01-02 15:04:05", record.Expiration, cz)
				if expiredTime.Unix() < now.Unix() {
					status = "已过期"
				}
			}
		}

		if option == "1" {
			if status == "已过期" || status == "已删除" {
				continue
			}
		}
		line := []string{strconv.Itoa(idx), record.ShareId, record.ShareUrl, record.SharePwd, record.ShareName, et, status}
		idx += 1
		columns = append(columns, line)
	}

	// save to file
	if ExportCsv(saveFilePath, columns) {
		fmt.Println("分享导出成功：", saveFilePath)
	}
}

func ExportCsv(savePath string, data [][]string) bool {
	folder := filepath.Dir(savePath)
	if _, err := os.Stat(folder); err != nil {
		if !os.IsExist(err) {
			os.MkdirAll(folder, os.ModePerm)
		}
	}
	fp, err := os.Create(savePath) // 创建文件句柄
	if err != nil {
		fmt.Println("创建文件["+savePath+"]失败,%v", err)
		return false
	}
	defer fp.Close()
	fp.WriteString("\xEF\xBB\xBF") // 写入UTF-8 BOM
	w := csv.NewWriter(fp)         //创建一个新的写入文件流
	w.WriteAll(data)
	w.Flush()
	return true
}
