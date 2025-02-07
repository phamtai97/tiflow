drop database if exists `dmctl`;
create database `dmctl`;
use `dmctl`;
create table t_1(id bigint auto_increment, b int, c varchar(20), d varchar(10), primary key id(id), unique key b(b));
create table t_2(id bigint auto_increment, b int, c varchar(20), d varchar(10), primary key id(id), unique key b(b));
INSERT INTO `dmctl`.`t_2` (`b`,`c`,`d`,`id`) VALUES (1795844527,'mpNYtz','JugWqaHw',1);
INSERT INTO `dmctl`.`t_2` (`b`,`c`,`d`,`id`) VALUES (816772144,'jjoPwqhBWpJyUUvgGWkp','FgPbiUqrvS',2);
INSERT INTO `dmctl`.`t_2` (`b`,`c`,`d`,`id`) VALUES (1058572812,'dCmAIAuZrNUJxBl','wiaFgp',3);
INSERT INTO `dmctl`.`t_1` (`b`,`c`,`d`,`id`) VALUES (1825468799,'DWzgtMAwUcoqZvupwm','GsusfUlbB',1);
INSERT INTO `dmctl`.`t_1` (`b`,`c`,`d`,`id`) VALUES (265700472,'rEsjuTsIS','JPTd',2);
INSERT INTO `dmctl`.`t_2` (`b`,`c`,`d`,`id`) VALUES (763390433,'TE','jbO',4);
INSERT INTO `dmctl`.`t_1` (`b`,`c`,`d`,`id`) VALUES (1112494892,'XDbXXvYTtJFLaF','zByU',3);
INSERT INTO `dmctl`.`t_2` (`b`,`c`,`d`,`id`) VALUES (61186151,'gXhXNtk','Hi',5);
INSERT INTO `dmctl`.`t_2` (`b`,`c`,`d`,`id`) VALUES (1190671373,'WGP','jUXxu',6);
INSERT INTO `dmctl`.`t_2` (`b`,`c`,`d`,`id`) VALUES (1192770284,'SyMVcUeK','MIZNFu',7);
INSERT INTO `dmctl`.`t_1` (`b`,`c`,`d`,`id`) VALUES (1647531504,'yNvqWnrbtTxc','ogSwAofM',4);
INSERT INTO `dmctl`.`t_1` (`b`,`c`,`d`,`id`) VALUES (1041099481,'zrO','C',5);
INSERT INTO `dmctl`.`t_2` (`b`,`c`,`d`,`id`) VALUES (1635431660,'pum','MMtT',8);
INSERT INTO `dmctl`.`t_1` (`b`,`c`,`d`,`id`) VALUES (208389298,'ZvhKh','Zt',6);
INSERT INTO `dmctl`.`t_2` (`b`,`c`,`d`,`id`) VALUES (2128788808,'hgWB','poUlMgBSX',9);
INSERT INTO `dmctl`.`t_2` (`b`,`c`,`d`,`id`) VALUES (1758036092,'CxSfGQNebY','OY',10);
INSERT INTO `dmctl`.`t_1` (`b`,`c`,`d`,`id`) VALUES (1649664004,'eIXDUjODpLjRkXu','NWlGjQq',7);
INSERT INTO `dmctl`.`t_1` (`b`,`c`,`d`,`id`) VALUES (1402446429,'xQMCGsfckXpoe','R',8);
INSERT INTO `dmctl`.`t_1` (`b`,`c`,`d`,`id`) VALUES (800180420,'JuUIxUacksp','sX',9);

create table tb_1(a INT, b INT);
create table tb_2(a INT, c INT);

CREATE TABLE only_warning (id bigint, b int, primary key id(id), FOREIGN KEY (b) REFERENCES t_1(b));