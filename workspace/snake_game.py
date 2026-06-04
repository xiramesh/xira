import pygame
import random
import sys

# 初始化 Pygame
pygame.init()

# 游戏窗口设置
WINDOW_WIDTH = 600
WINDOW_HEIGHT = 600
GRID_SIZE = 20
CELL_SIZE = WINDOW_WIDTH // GRID_SIZE  # 每个格子的大小

# 颜色定义
BLACK = (0, 0, 0)
WHITE = (255, 255, 255)
GREEN = (0, 255, 0)
RED = (255, 0, 0)
DARK_GREEN = (0, 200, 0)
GRAY = (200, 200, 200)

# 方向常量
UP = (0, -1)
DOWN = (0, 1)
LEFT = (-1, 0)
RIGHT = (1, 0)

class Snake:
    def __init__(self):
        # 蛇的初始位置在屏幕中央
        self.positions = [(GRID_SIZE // 2, GRID_SIZE // 2)]
        self.direction = RIGHT
        self.grow_flag = False

    def move(self):
        head_x, head_y = self.positions[0]
        dx, dy = self.direction
        new_head = ((head_x + dx) % GRID_SIZE, (head_y + dy) % GRID_SIZE)

        # 检查是否撞到自己
        if new_head in self.positions:
            return False  # 游戏结束

        self.positions.insert(0, new_head)
        if not self.grow_flag:
            self.positions.pop()
        else:
            self.grow_flag = False
        return True

    def grow(self):
        self.grow_flag = True

    def change_direction(self, new_direction):
        # 不允许直接反向
        if (new_direction[0] * -1, new_direction[1] * -1) != self.direction:
            self.direction = new_direction

class Food:
    def __init__(self, snake_positions):
        self.position = self.random_position(snake_positions)

    def random_position(self, snake_positions):
        while True:
            pos = (random.randint(0, GRID_SIZE - 1), random.randint(0, GRID_SIZE - 1))
            if pos not in snake_positions:
                return pos

    def respawn(self, snake_positions):
        self.position = self.random_position(snake_positions)

class Game:
    def __init__(self):
        self.screen = pygame.display.set_mode((WINDOW_WIDTH, WINDOW_HEIGHT))
        pygame.display.set_caption("贪吃蛇 - Snake Game")
        self.clock = pygame.time.Clock()
        self.font_large = pygame.font.Font(None, 72)
        self.font_small = pygame.font.Font(None, 36)
        self.reset_game()

    def reset_game(self):
        self.snake = Snake()
        self.food = Food(self.snake.positions)
        self.score = 0
        self.game_over = False
        self.paused = False

    def draw_grid(self):
        for x in range(0, WINDOW_WIDTH, CELL_SIZE):
            pygame.draw.line(self.screen, GRAY, (x, 0), (x, WINDOW_HEIGHT), 1)
        for y in range(0, WINDOW_HEIGHT, CELL_SIZE):
            pygame.draw.line(self.screen, GRAY, (0, y), (WINDOW_WIDTH, y), 1)

    def draw_snake(self):
        for i, pos in enumerate(self.snake.positions):
            x, y = pos
            rect = pygame.Rect(x * CELL_SIZE, y * CELL_SIZE, CELL_SIZE, CELL_SIZE)
            if i == 0:
                pygame.draw.rect(self.screen, DARK_GREEN, rect)
                pygame.draw.rect(self.screen, GREEN, rect.inflate(-4, -4))
            else:
                pygame.draw.rect(self.screen, GREEN, rect)
                pygame.draw.rect(self.screen, DARK_GREEN, rect.inflate(-4, -4))

    def draw_food(self):
        x, y = self.food.position
        rect = pygame.Rect(x * CELL_SIZE, y * CELL_SIZE, CELL_SIZE, CELL_SIZE)
        pygame.draw.rect(self.screen, RED, rect)
        pygame.draw.circle(self.screen, (200, 0, 0), rect.center, CELL_SIZE // 3)

    def draw_score(self):
        score_text = self.font_small.render(f"得分: {self.score}", True, WHITE)
        self.screen.blit(score_text, (10, 10))

    def draw_game_over(self):
        overlay = pygame.Surface((WINDOW_WIDTH, WINDOW_HEIGHT))
        overlay.set_alpha(128)
        overlay.fill(BLACK)
        self.screen.blit(overlay, (0, 0))

        game_over_text = self.font_large.render("游戏结束", True, RED)
        text_rect = game_over_text.get_rect(center=(WINDOW_WIDTH // 2, WINDOW_HEIGHT // 2 - 50))
        self.screen.blit(game_over_text, text_rect)

        score_text = self.font_small.render(f"最终得分: {self.score}", True, WHITE)
        score_rect = score_text.get_rect(center=(WINDOW_WIDTH // 2, WINDOW_HEIGHT // 2 + 10))
        self.screen.blit(score_text, score_rect)

        restart_text = self.font_small.render("按 R 键重新开始  按 Q 键退出", True, WHITE)
        restart_rect = restart_text.get_rect(center=(WINDOW_WIDTH // 2, WINDOW_HEIGHT // 2 + 60))
        self.screen.blit(restart_text, restart_rect)

    def draw_pause(self):
        pause_text = self.font_large.render("暂停", True, WHITE)
        text_rect = pause_text.get_rect(center=(WINDOW_WIDTH // 2, WINDOW_HEIGHT // 2))
        self.screen.blit(pause_text, text_rect)

        hint_text = self.font_small.render("按 P 键继续", True, WHITE)
        hint_rect = hint_text.get_rect(center=(WINDOW_WIDTH // 2, WINDOW_HEIGHT // 2 + 50))
        self.screen.blit(hint_text, hint_rect)

    def handle_events(self):
        for event in pygame.event.get():
            if event.type == pygame.QUIT:
                return False
            if event.type == pygame.KEYDOWN:
                if event.key == pygame.K_UP:
                    self.snake.change_direction(UP)
                elif event.key == pygame.K_DOWN:
                    self.snake.change_direction(DOWN)
                elif event.key == pygame.K_LEFT:
                    self.snake.change_direction(LEFT)
                elif event.key == pygame.K_RIGHT:
                    self.snake.change_direction(RIGHT)
                elif event.key == pygame.K_p:
                    self.paused = not self.paused
                elif event.key == pygame.K_r and self.game_over:
                    self.reset_game()
                elif event.key == pygame.K_q and self.game_over:
                    return False
        return True

    def update(self):
        if self.game_over or self.paused:
            return
        if not self.snake.move():
            self.game_over = True
            return
        if self.snake.positions[0] == self.food.position:
            self.snake.grow()
            self.score += 1
            self.food.respawn(self.snake.positions)

    def render(self):
        self.screen.fill(BLACK)
        self.draw_grid()
        self.draw_snake()
        self.draw_food()
        self.draw_score()
        if self.game_over:
            self.draw_game_over()
        elif self.paused:
            self.draw_pause()
        pygame.display.flip()

    def run(self):
        running = True
        while running:
            running = self.handle_events()
            self.update()
            self.render()
            self.clock.tick(10)
        pygame.quit()
        sys.exit()

if __name__ == "__main__":
    game = Game()
    game.run()
