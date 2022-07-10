<?php
declare(strict_types=1);

namespace App\Command;

use App\Repository\UserRepository;
use App\User\{InviteManager, UserManager};
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Component\Console\Command\Command;
use Symfony\Component\Console\Helper\QuestionHelper;
use Symfony\Component\Console\Input\{InputArgument, InputInterface, InputOption};
use Symfony\Component\Console\Output\OutputInterface;
use Symfony\Component\Console\Question\Question;

class AddUserCommand extends Command
{
    public function __construct(
        private readonly EntityManagerInterface $em,
        private readonly UserManager $userManager,
        private readonly UserRepository $userRepo,
        private readonly InviteManager $inviteManager,
    ) {
        parent::__construct();
    }

    protected function configure()
    {
        $this
            ->setName('user:add')
            ->addArgument('username', InputArgument::REQUIRED, 'Username')
            ->addArgument('email', InputArgument::REQUIRED, 'Email')
            ->addArgument('password', InputArgument::OPTIONAL, 'Password', null)
            ->addOption('invites', 'i', InputOption::VALUE_OPTIONAL, 'Number of invites for user', 0)
            ->addOption('role', 'r', InputOption::VALUE_IS_ARRAY | InputOption::VALUE_OPTIONAL, 'Role to add to the user')
        ;
    }

    protected function execute(InputInterface $input, OutputInterface $output): int
    {
        $username = $input->getArgument('username');
        $email = $input->getArgument('email');
        $password = $input->getArgument('password');
        $invites = (int) $input->getOption('invites');
        $roles = (array) $input->getOption('role');

        if (!$password) {
            /** @var QuestionHelper $questionHelper */
            $questionHelper = $this->getHelper('question');
            $question = new Question('Enter new user\'s password: ');
            $question->setHidden(true);
            $question->setHiddenFallback(false);

            $password = $questionHelper->ask($input, $output, $question);
        }

        if (!$password) {
            $output->writeln('<error>User password cannot be empty.</error>');

            return Command::FAILURE;
        }

        if ($roles) {
            $user = $this->userManager->createUser($username, $password, $email, $roles);
        } else {
            $user = $this->userManager->createUser($username, $password, $email);
        }


        $this->userRepo->add($user);

        if ($invites) {
            $this->inviteManager->createInvitesForUser($user, $invites);
        }

        $this->em->flush();

        $output->writeln(sprintf('<info>User \'%s\' registered, %d invites added.</info>', $user->getUsername(), $invites));

        return Command::SUCCESS;
    }

}